// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysqldump"
	"github.com/logisticinfotech/liops/app/engines/xtrabackup"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// newMySQLWorld is a fake MySQL server with the given databases. Only the mysql and
// mysqldump programs exist on it.
func newMySQLWorld(t *testing.T, dbs ...string) *world {
	t.Helper()
	w := &world{databases: dbs, failDump: map[string]bool{}, now: time.Date(2026, 10, 13, 1, 30, 0, 0, time.UTC)}
	cfg := config.Default()
	cfg.Engine = config.EngineMySQL
	cfg.Server.Timezone = "UTC"
	cfg.MySQL.BackupName = "shop-db-1"
	cfg.MySQL.DataDir = t.TempDir()
	w.binlogDir = t.TempDir()
	w.addBinlog("binlog.000003")

	st, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	w.store = storage.NewMemory()
	w.store.Now = w.clock
	w.mail = &notify.Recorder{}
	w.fake = &command.Fake{Handle: func(program string, c command.Cmd) ([]byte, error) {
		query := c.Args[len(c.Args)-1]
		switch {
		case program == "xtrabackup":
			return fakeXtraBackup(c)
		case program == "mysqldump":
			return []byte("-- MySQL dump of " + query + "\n"), nil
		case program == "mysql" && strings.Contains(query, "information_schema.SCHEMATA"):
			var out strings.Builder
			for _, db := range append([]string{"mysql", "sys"}, w.databases...) {
				out.WriteString(strings.ToUpper(hex.EncodeToString([]byte(db))) + "\n")
			}
			return []byte(out.String()), nil
		case program == "mysql" && strings.Contains(query, "FROM mysql.user"):
			return []byte(hex.EncodeToString([]byte("app")) + "\t" + hex.EncodeToString([]byte("%")) + "\n"), nil
		case program == "mysql" && strings.Contains(query, "BINARY LOG"), program == "mysql" && strings.Contains(query, "log_bin_basename"):
			return w.binlogQuery(strings.TrimPrefix(query, "--execute="))
		case program == "mysql" && strings.Contains(query, "SHOW CREATE USER"):
			return []byte("CREATE USER `app`@`%` IDENTIFIED WITH 'caching_sha2_password' AS 0x24\nGRANT USAGE ON *.* TO `app`@`%`\n"), nil
		}
		return nil, fmt.Errorf("unexpected command %s %q", program, c.Args)
	}}
	w.runner = &Runner{
		Cfg:        cfg,
		Server:     "mysql-test",
		State:      st,
		Store:      w.store,
		Mail:       w.mail,
		XtraBackup: xtrabackup.New(cfg, w.fake, w.store, t.TempDir()),
		Dumps:      mysqldump.New(cfg, w.fake, w.store),
		LockPath:   filepath.Join(t.TempDir(), "job.lock"),
		Now:        w.clock,
		Log:        &w.log,
	}
	return w
}

// addBinlog starts a new binlog file, as FLUSH BINARY LOGS does.
func (w *world) addBinlog(name string) {
	os.WriteFile(filepath.Join(w.binlogDir, name), []byte("events of "+name), 0o600)
	w.binlogs = append(w.binlogs, name)
	w.binlogPos = 158
}

// write makes MySQL write something to the current binlog.
func (w *world) write() { w.binlogPos += 100 }

func (w *world) binlogQuery(q string) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failBinlogs {
		return nil, &command.Error{Cmd: "mysql", ExitCode: 1, Output: "ERROR 2002 (HY000): Can't connect to local MySQL server through socket", Err: errors.New("exit status 1")}
	}
	current := w.binlogs[len(w.binlogs)-1]
	switch q {
	case "SHOW BINARY LOG STATUS":
		return fmt.Appendf(nil, "%s\t%d\t\t\ta102517a-d2e9-11f1-b24f-525400707196:1-9\n", current, w.binlogPos), nil
	case "FLUSH BINARY LOGS":
		var n int
		fmt.Sscanf(current[len("binlog."):], "%d", &n)
		w.addBinlog(fmt.Sprintf("binlog.%06d", n+1))
		return nil, nil
	case "SHOW BINARY LOGS":
		var b strings.Builder
		for _, name := range w.binlogs {
			if data, err := os.ReadFile(filepath.Join(w.binlogDir, name)); err == nil {
				fmt.Fprintf(&b, "%s\t%d\tNo\n", name, len(data))
			}
		}
		return []byte(b.String()), nil
	case "SELECT @@log_bin_basename":
		return []byte(filepath.Join(w.binlogDir, "binlog") + "\n"), nil
	}
	return nil, fmt.Errorf("unexpected binlog query %q", q)
}

func TestMySQLBinlogs(t *testing.T) {
	w := newMySQLWorld(t, "app")
	ctx := context.Background()

	// No backup yet: nothing to replay binlogs onto, so nothing is closed or shipped.
	if err := w.runner.Run(ctx, Binlogs, Scheduled); err != nil {
		t.Fatal(err)
	}
	if len(w.binlogs) != 1 || len(w.keys(xtrabackup.BinlogPrefix)) != 0 {
		t.Errorf("before any backup: binlogs %q, stored %q", w.binlogs, w.keys(xtrabackup.BinlogPrefix))
	}

	// The full backup is consistent with binlog.000003; after writes, shipping closes it.
	if err := w.runner.Run(ctx, Full, Manual); err != nil {
		t.Fatal(err)
	}
	w.write()
	if err := w.runner.Run(ctx, Binlogs, Scheduled); err != nil {
		t.Fatalf("binlogs: %v\n%s", err, w.log.String())
	}
	if got := w.keys(xtrabackup.BinlogPrefix); !slices.Equal(got, []string{"xtrabackup/binlog/binlog.000003.gz"}) {
		t.Errorf("stored binlogs = %q", got)
	}
	if v, _ := w.runner.State.GetMeta(MetaBinlogShippedAt); v == "" {
		t.Error("a run that worked should be recorded in the state meta")
	}

	// Nothing written since: MySQL isn't asked to close a binlog every few minutes.
	before := len(w.binlogs)
	if err := w.runner.Run(ctx, Binlogs, Scheduled); err != nil || len(w.binlogs) != before {
		t.Errorf("a quiet server was rotated: %v, %q", err, w.binlogs)
	}
	runs, _ := w.runner.State.Runs(Binlogs, 10)
	if len(runs) != 0 {
		t.Errorf("binlog runs mustn't fill the job history: %d", len(runs))
	}
}

func TestMySQLBinlogFailuresEmailOnce(t *testing.T) {
	w := newMySQLWorld(t, "app")
	ctx := context.Background()
	if err := w.runner.Run(ctx, Full, Manual); err != nil {
		t.Fatal(err)
	}
	w.failBinlogs = true
	for range 3 {
		if err := w.runner.Run(ctx, Binlogs, Scheduled); err == nil {
			t.Fatal("want an error")
		}
	}
	if subjects := w.subjects(); len(subjects) != 1 || !strings.Contains(subjects[0], "binlog shipping FAILED") {
		t.Errorf("three failed runs should send one email: %q", subjects)
	}
	w.failBinlogs = false
	w.write()
	if err := w.runner.Run(ctx, Binlogs, Scheduled); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.log.String(), "shipping works again") {
		t.Errorf("recovery should be logged:\n%s", w.log.String())
	}
	w.failBinlogs = true
	w.runner.Run(ctx, Binlogs, Scheduled)
	if len(w.subjects()) != 2 {
		t.Errorf("a new failure after a recovery should email again: %q", w.subjects())
	}
}

func TestMySQLBinlogGapTakesAFullBackup(t *testing.T) {
	w := newMySQLWorld(t, "app")
	ctx := context.Background()
	if err := w.runner.Run(ctx, Full, Manual); err != nil {
		t.Fatal(err)
	}
	// binlog.000003 is purged from the server before it is shipped.
	w.write()
	w.binlogQuery("FLUSH BINARY LOGS")
	os.Remove(filepath.Join(w.binlogDir, "binlog.000003"))
	if err := w.runner.Run(ctx, Binlogs, Scheduled); err != nil {
		t.Fatalf("binlogs: %v\n%s", err, w.log.String())
	}
	if subjects := w.subjects(); len(subjects) != 1 || !strings.Contains(subjects[0], "binlogs missing") {
		t.Errorf("emails = %q", subjects)
	}
	runs, _ := w.runner.State.Runs(Full, 10)
	if len(runs) != 2 || runs[0].Trigger != Automatic {
		t.Errorf("a gap should start a full backup: %+v", runs)
	}
	// The same gap isn't reported twice.
	w.runner.Run(ctx, Binlogs, Scheduled)
	if len(w.subjects()) != 1 {
		t.Errorf("emails = %q", w.subjects())
	}
}

func TestBinlogsOnlyOnMySQL(t *testing.T) {
	w := newWorld(t, "app")
	if err := w.runner.Run(context.Background(), Binlogs, Manual); err == nil || !strings.Contains(err.Error(), "only shipped for MySQL") {
		t.Errorf("got %v", err)
	}
}

func TestMySQLDumps(t *testing.T) {
	w := newMySQLWorld(t, "app", "tenant_abc")
	if err := w.runner.Run(context.Background(), Dumps, Manual); err != nil {
		t.Fatalf("dumps: %v\n%s", err, w.log.String())
	}
	keys := w.keys(storage.DumpsPrefix)
	if len(keys) != 2 || !strings.HasPrefix(keys[0], "dumps/app/") || !strings.HasSuffix(keys[0], ".sql.gz") {
		t.Errorf("dump keys = %q", keys)
	}
	globals := w.keys(storage.GlobalsPrefix)
	if len(globals) != 1 {
		t.Fatalf("globals = %q", globals)
	}
	if data, _ := w.store.Get(globals[0]); !strings.Contains(string(data), "CREATE USER IF NOT EXISTS `app`@`%`") {
		t.Errorf("users and grants = %q", data)
	}
	if programs := w.fake.Programs(); slices.Contains(programs, "pgbackrest") || slices.Contains(programs, "psql") {
		t.Errorf("a MySQL server must not run PostgreSQL programs: %q", programs)
	}
}

// fakeXtraBackup writes XtraBackup's stream, and its metadata into --extra-lsndir.
func fakeXtraBackup(c command.Cmd) ([]byte, error) {
	for _, a := range c.Args {
		if dir, ok := strings.CutPrefix(a, "--extra-lsndir="); ok {
			os.WriteFile(filepath.Join(dir, "xtrabackup_checkpoints"), []byte("from_lsn = 0\nto_lsn = 5000\n"), 0o600)
			os.WriteFile(filepath.Join(dir, "xtrabackup_info"), []byte("server_version = 8.4.11\ntool_version = 8.4.0-7\n"+
				"binlog_pos = filename 'binlog.000003', position '158', GTID of the last change 'a102517a-d2e9-11f1-b24f-525400707196:1-9'\n"), 0o600)
		}
	}
	return []byte("XBSTREAM"), nil
}

func TestMySQLFullThenDiff(t *testing.T) {
	w := newMySQLWorld(t, "app")
	ctx := context.Background()
	if err := w.runner.Run(ctx, Full, Manual); err != nil {
		t.Fatalf("full: %v\n%s", err, w.log.String())
	}
	if err := w.runner.Run(ctx, Diff, Manual); err != nil {
		t.Fatalf("diff: %v\n%s", err, w.log.String())
	}
	list, err := w.runner.XtraBackup.Backups(ctx)
	if err != nil || len(list) != 2 || list[0].Type != xtrabackup.Full || list[1].Base != list[0].Label {
		t.Fatalf("backups = %+v, %v", list, err)
	}
	// Each backup is followed by the dumps, as on PostgreSQL.
	runs, _ := w.runner.State.Runs("", 10)
	var jobs []string
	for _, r := range runs {
		jobs = append(jobs, r.Job)
	}
	if !slices.Equal(jobs, []string{Dumps, Diff, Dumps, Full}) {
		t.Errorf("runs, newest first = %q", jobs)
	}
	if programs := w.fake.Programs(); slices.Contains(programs, "pgbackrest") || slices.Contains(programs, "psql") {
		t.Errorf("a MySQL server must not run PostgreSQL programs: %q", programs)
	}
}

func TestMySQLReport(t *testing.T) {
	w := newMySQLWorld(t, "app")
	if err := w.runner.Run(context.Background(), Dumps, Manual); err != nil {
		t.Fatal(err)
	}
	if err := w.runner.Run(context.Background(), Report, Manual); err != nil {
		t.Fatalf("report: %v\n%s", err, w.log.String())
	}
	if len(w.mail.Sent) != 1 {
		t.Fatalf("emails = %v", w.subjects())
	}
	body := w.mail.Sent[0].Body
	if !strings.Contains(w.mail.Sent[0].Subject, "backups healthy") || strings.Contains(body, "Point-in-time") {
		t.Errorf("summary for a healthy MySQL server:\n%s\n%s", w.mail.Sent[0].Subject, body)
	}
}

func TestMySQLSchedule(t *testing.T) {
	w := newMySQLWorld(t, "app")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := w.runner.StartScheduler(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	var jobs []string
	for _, n := range s.Next() {
		jobs = append(jobs, n.Job)
	}
	slices.Sort(jobs)
	if !slices.Equal(jobs, []string{Binlogs, Diff, Full, Report}) {
		t.Errorf("scheduled jobs = %q, want binlogs, full, diff and the report", jobs)
	}
}
