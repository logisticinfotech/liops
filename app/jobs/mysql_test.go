// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysqldump"
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
		case program == "mysql" && strings.Contains(query, "SHOW CREATE USER"):
			return []byte("CREATE USER `app`@`%` IDENTIFIED WITH 'caching_sha2_password' AS 0x24\nGRANT USAGE ON *.* TO `app`@`%`\n"), nil
		}
		return nil, fmt.Errorf("unexpected command %s %q", program, c.Args)
	}}
	w.runner = &Runner{
		Cfg:      cfg,
		Server:   "mysql-test",
		State:    st,
		Store:    w.store,
		Mail:     w.mail,
		Dumps:    mysqldump.New(cfg, w.fake, w.store),
		LockPath: filepath.Join(t.TempDir(), "job.lock"),
		Now:      w.clock,
		Log:      &w.log,
	}
	return w
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

func TestMySQLHasNoPointInTimeBackupsYet(t *testing.T) {
	w := newMySQLWorld(t, "app")
	err := w.runner.Run(context.Background(), Full, Manual)
	if err == nil || !strings.Contains(err.Error(), "liops run dumps") {
		t.Errorf("full on MySQL should explain what to do instead, got %v", err)
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
	if !slices.Equal(jobs, []string{Dumps, Dumps, Report}) {
		t.Errorf("scheduled jobs = %q, want dumps at the full and diff times, and the report", jobs)
	}
}
