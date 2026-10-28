// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/engines/pgdump"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// world is a fake server: databases, pgBackRest and storage, all in memory.
type world struct {
	mu        sync.Mutex
	databases []string
	failDump  map[string]bool
	failBack  bool
	now       time.Time

	// MySQL's binlogs: files in binlogDir, the last one being written up to binlogPos.
	binlogDir   string
	binlogs     []string
	binlogPos   int
	failBinlogs bool
	// mysqlSettings answers SELECT @@log_bin, @@binlog_format, @@gtid_mode, ...
	mysqlSettings string

	runner *Runner
	store  *storage.Memory
	mail   *notify.Recorder
	fake   *command.Fake
	log    bytes.Buffer
}

func newWorld(t *testing.T, dbs ...string) *world {
	t.Helper()
	w := &world{databases: dbs, failDump: map[string]bool{}, now: time.Date(2026, 10, 13, 1, 30, 0, 0, time.UTC)}
	cfg := config.Default()
	cfg.Server.Timezone = "UTC"
	cfg.PgBackRest.Stanza = "main"

	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	w.store = storage.NewMemory()
	w.store.Now = w.clock
	w.mail = &notify.Recorder{}
	w.fake = &command.Fake{Handle: w.handle}
	w.runner = &Runner{
		Cfg:      cfg,
		Server:   "db-test",
		State:    st,
		Store:    w.store,
		Mail:     w.mail,
		Backrest: pgbackrest.New(cfg, w.fake),
		Dumps:    pgdump.New(cfg, w.fake, w.store),
		Cluster:  postgres.Cluster{Cfg: cfg.Postgres, Run: w.fake},
		LockPath: filepath.Join(t.TempDir(), "job.lock"),
		Now:      w.clock,
		Log:      &w.log,
	}
	return w
}

// clock returns the fake time and advances it a little, so each dump gets its own key.
func (w *world) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(time.Second)
	return w.now
}

func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	w.now = w.now.Add(d)
	w.mu.Unlock()
}

func (w *world) handle(program string, c command.Cmd) ([]byte, error) {
	args := strings.Join(c.Args, " ")
	switch program {
	case "pgbackrest":
		switch {
		case strings.HasSuffix(args, " backup"):
			if w.failBack {
				out := "ERROR: [039]: HTTP request failed with 403 (Forbidden)\n"
				return []byte(out), &command.Error{Cmd: "pgbackrest", ExitCode: 39, Output: out, Err: errors.New("exit status 39")}
			}
			return []byte("INFO: backup command end: completed successfully\n"), nil
		case strings.HasSuffix(args, " info"):
			stop := w.now.Add(-time.Hour).Unix()
			return fmt.Appendf(nil, `[{"name":"main","status":{"code":0,"message":"ok"},"archive":[],
				"backup":[{"label":"F","type":"full","timestamp":{"start":%d,"stop":%d},"info":{"size":100,"repository":{"delta":90}}},
				          {"label":"F_D","type":"diff","timestamp":{"start":%d,"stop":%d},"info":{"size":100,"repository":{"delta":42}}}]}]`,
				stop-86436, stop-86400, stop-36, stop), nil
		case strings.HasSuffix(args, " check"):
			return []byte("INFO: check command end: completed successfully\n"), nil
		}
	case "psql":
		if strings.Contains(args, "pg_stat_archiver") {
			return fmt.Appendf(nil, "%d\t0\n", w.now.Unix()), nil
		}
		var out strings.Builder
		for _, db := range w.databases {
			out.WriteString(hex.EncodeToString([]byte(db)) + "\n")
		}
		return []byte(out.String()), nil
	case "pg_dump":
		for db := range w.failDump {
			if strings.Contains(args, postgres.Conn(db)) {
				out := "pg_dump: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: Connection refused\n"
				return nil, &command.Error{Cmd: "pg_dump", ExitCode: 1, Output: out, Err: errors.New("exit status 1")}
			}
		}
		return []byte("PGDMP " + args), nil
	case "pg_dumpall":
		return []byte("CREATE ROLE app;\n"), nil
	}
	return nil, fmt.Errorf("unexpected command %s %s", program, args)
}

func (w *world) keys(prefix string) []string {
	objs, _ := w.store.List(context.Background(), prefix)
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	return keys
}

func (w *world) subjects() []string {
	var s []string
	for _, e := range w.mail.Sent {
		s = append(s, e.Subject)
	}
	return s
}

func TestDiffThenDumps(t *testing.T) {
	w := newWorld(t, "app", "postgres", "tenant_abc")
	if err := w.runner.Run(context.Background(), Diff, Scheduled); err != nil {
		t.Fatalf("Run: %v\n%s", err, w.log.String())
	}
	runs, _ := w.runner.State.Runs("", 10)
	if len(runs) != 2 || runs[1].Job != Diff || runs[0].Job != Dumps || runs[0].Status != state.StatusOK || runs[1].Size != 42 {
		t.Fatalf("expected diff then dumps, both ok: %+v", runs)
	}
	if got := len(runs[0].Dumps); got != 2 {
		t.Errorf("postgres is excluded by default, so 2 databases are dumped, got %d", got)
	}
	if len(w.keys("dumps/app/")) != 1 || len(w.keys("dumps/tenant_abc/")) != 1 || len(w.keys("globals/")) != 1 {
		t.Errorf("stored: %v", w.keys(""))
	}
	if len(w.mail.Sent) != 0 {
		t.Errorf("a first, successful run sends no email: %v", w.subjects())
	}
}

func TestFailedDumpSendsOneEmail(t *testing.T) {
	w := newWorld(t, "app", "tenant_xyz")
	if err := w.runner.Run(context.Background(), Dumps, Manual); err != nil {
		t.Fatal(err)
	}
	w.advance(24 * time.Hour)
	w.failDump["tenant_xyz"] = true

	err := w.runner.Run(context.Background(), Dumps, Scheduled)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 database dump(s) failed") {
		t.Fatalf("expected a dump failure, got %v", err)
	}
	if len(w.mail.Sent) != 1 {
		t.Fatalf("expected one failure email, got %v", w.subjects())
	}
	email := w.mail.Sent[0]
	if email.Subject != "❌ db-test — tonight's database dumps FAILED for 1 database" {
		t.Errorf("subject = %q", email.Subject)
	}
	for _, want := range []string{"dump of tenant_xyz", "is PostgreSQL running?", "tenant_xyz's last good dump is from 13 Oct"} {
		if !strings.Contains(email.Body, want) {
			t.Errorf("body is missing %q:\n%s", want, email.Body)
		}
	}
	if len(w.keys("dumps/app/")) != 2 || len(w.keys("dumps/tenant_xyz/")) != 1 {
		t.Errorf("app is dumped again, tenant_xyz keeps its old dump: %v", w.keys("dumps/"))
	}
	last, _ := w.runner.State.LastRun(Dumps, "")
	if last.Status != state.StatusFailed {
		t.Errorf("run status = %q", last.Status)
	}
}

func TestNewAndDeletedDatabases(t *testing.T) {
	w := newWorld(t, "app", "old_tenant")
	if err := w.runner.Run(context.Background(), Dumps, Manual); err != nil {
		t.Fatal(err)
	}

	w.advance(24 * time.Hour)
	w.databases = []string{"app", "new_tenant"}
	if err := w.runner.Run(context.Background(), Dumps, Scheduled); err != nil {
		t.Fatal(err)
	}
	subjects := w.subjects()
	if !slices.ContainsFunc(subjects, func(s string) bool { return strings.Contains(s, "new database found: new_tenant") }) ||
		!slices.ContainsFunc(subjects, func(s string) bool { return strings.Contains(s, "database no longer found: old_tenant") }) {
		t.Fatalf("emails: %v", subjects)
	}
	deleted, _ := w.runner.State.Deleted()
	if _, ok := deleted["old_tenant"]; !ok {
		t.Fatal("old_tenant should be tracked as deleted")
	}

	// After the deleted-database retention (14 days), its last dump goes.
	w.advance(15 * 24 * time.Hour)
	if err := w.runner.Run(context.Background(), Dumps, Scheduled); err != nil {
		t.Fatal(err)
	}
	if keys := w.keys("dumps/old_tenant/"); len(keys) != 0 {
		t.Errorf("old_tenant's dump should be gone: %v", keys)
	}
	if deleted, _ := w.runner.State.Deleted(); len(deleted) != 0 {
		t.Errorf("old_tenant should no longer be tracked: %v", deleted)
	}
	// Live databases keep the newest dump plus the ones within 7 days.
	if keys := w.keys("dumps/app/"); len(keys) != 1 {
		t.Errorf("app should have only its newest dump left: %v", keys)
	}
}

func TestBackupFailureEmail(t *testing.T) {
	w := newWorld(t, "app")
	w.runner.Cfg.Dumps.Enabled = false
	w.failBack = true
	if err := w.runner.Run(context.Background(), Full, Scheduled); err == nil {
		t.Fatal("expected the backup to fail")
	}
	if len(w.mail.Sent) != 1 {
		t.Fatalf("emails: %v", w.subjects())
	}
	email := w.mail.Sent[0]
	if email.Subject != "❌ db-test — tonight's full backup FAILED" || !strings.Contains(email.Body, "the storage key was rejected") ||
		!strings.Contains(email.Body, "the newest good one is the diff from") {
		t.Errorf("email:\n%s\n%s", email.Subject, email.Body)
	}
}

func TestReport(t *testing.T) {
	w := newWorld(t, "app")
	if err := w.runner.Run(context.Background(), Dumps, Manual); err != nil {
		t.Fatal(err)
	}
	w.advance(6 * time.Hour)
	if err := w.runner.Run(context.Background(), Report, Scheduled); err != nil {
		t.Fatal(err)
	}
	if len(w.mail.Sent) != 1 {
		t.Fatalf("emails: %v", w.subjects())
	}
	email := w.mail.Sent[0]
	if !strings.HasPrefix(email.Subject, "✅ db-test — backups healthy") {
		t.Errorf("expected a healthy summary:\n%s\n%s", email.Subject, email.Body)
	}
	if !strings.Contains(email.Body, "1 of 1 databases dumped tonight") || !strings.Contains(email.Body, "Next full backup: Sun 18 Oct 01:30") {
		t.Errorf("body:\n%s", email.Body)
	}
}

func TestReportFlagsMissedBackup(t *testing.T) {
	w := newWorld(t, "app")
	w.runner.Backrest = pgbackrest.New(w.runner.Cfg, &command.Fake{Handle: func(program string, c command.Cmd) ([]byte, error) {
		if strings.HasSuffix(strings.Join(c.Args, " "), " info") {
			return []byte(`[{"name":"main","status":{"code":2,"message":"no valid backups"},"backup":[]}]`), nil
		}
		return nil, nil
	}})
	if err := w.runner.Run(context.Background(), Report, Scheduled); err != nil {
		t.Fatal(err)
	}
	body := w.mail.Sent[0].Body
	if !strings.Contains(w.mail.Sent[0].Subject, "need attention") || !strings.Contains(body, "didn't complete") || !strings.Contains(body, "no database dumps have run yet") {
		t.Errorf("summary should flag the missed backup and missing dumps:\n%s", body)
	}
}

func TestLockQueuesJobs(t *testing.T) {
	w := newWorld(t)
	unlock, err := w.runner.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := w.runner.lock(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second job should wait for the lock, got %v", err)
	}
	unlock()
	unlock2, err := w.runner.lock(context.Background())
	if err != nil {
		t.Fatalf("lock should be free again: %v", err)
	}
	unlock2()
}

func TestUnknownJob(t *testing.T) {
	w := newWorld(t)
	if err := w.runner.Run(context.Background(), "restore", Manual); err == nil {
		t.Fatal("expected an error")
	}
}

func TestProgressHelpers(t *testing.T) {
	if (Progress{}).Active() || !(Progress{Kind: "dumps"}).Active() {
		t.Fatal("Active should follow Kind")
	}
	if !(Progress{Total: 3}).Determinate() || (Progress{}).Determinate() {
		t.Fatal("Determinate should follow Total > 0")
	}
}

func TestDumpsReportsProgress(t *testing.T) {
	w := newWorld(t, "app", "tenant_abc")
	if err := w.runner.Run(context.Background(), Dumps, Manual); err != nil {
		t.Fatal(err)
	}
	// Finished dumps clear progress.
	if p := w.runner.Progress(); p.Kind != "" {
		t.Errorf("progress should be cleared after dumps: %+v", p)
	}
}

func TestBackupProgressFromPgBackRest(t *testing.T) {
	w := newWorld(t)
	old := backupPollInterval
	backupPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { backupPollInterval = old })

	seen := make(chan Progress, 1)
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		if strings.HasSuffix(strings.Join(c.Args, " "), " info") {
			return []byte(`[{"name":"main","archive":[],"backup":[],"status":{"code":2,"message":"no valid backups",
				"lock":{"backup":{"held":true,"size":4096,"size-cplt":1024}}}}]`), nil
		}
		// The backup itself: keep running until the bar shows pgBackRest's numbers.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if p := w.runner.Progress(); p.Bytes {
				seen <- p
				break
			}
		}
		return []byte("INFO: backup command end: completed successfully\n"), nil
	}}
	w.runner.Backrest = pgbackrest.New(w.runner.Cfg, fake)

	if err := w.runner.backup(context.Background(), Full, Manual, &state.Run{}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-seen:
		if p.Current != 1024 || p.Total != 4096 || p.Percent() != 25 || !strings.Contains(p.Detail, " of ") {
			t.Errorf("progress = %+v", p)
		}
	default:
		t.Fatal("the progress bar never showed pgBackRest's progress")
	}
	if w.runner.Progress().Active() {
		t.Error("progress should be cleared after the backup")
	}
}
