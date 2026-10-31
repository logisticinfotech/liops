// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysql"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/config"
)

func TestPostgresMonitorAccess(t *testing.T) {
	cfg := config.Default()
	cfg.Postgres.Layout, cfg.Postgres.DataDir = config.LayoutHomebrew, t.TempDir()
	libs, reloaded := "timescaledb", false
	var sqls []string
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		sql := c.Args[len(c.Args)-1]
		sqls = append(sqls, sql)
		switch {
		case strings.Contains(sql, "current_setting('shared_preload_libraries')"):
			return []byte("libs:" + libs + "\n"), nil
		case strings.Contains(sql, "pg_reload_conf"):
			reloaded = true
			return []byte("t\n"), nil
		case strings.Contains(sql, "pending_restart"):
			return []byte("t\t0\n"), nil
		}
		return nil, nil
	}}
	c := postgres.Cluster{Cfg: cfg.Postgres, Run: fake}
	restart, err := PostgresMonitorAccess(context.Background(), c)
	if err != nil || !restart || !reloaded {
		t.Fatalf("restart %v, reloaded %v, err %v", restart, reloaded, err)
	}
	conf, err := os.ReadFile(filepath.Join(cfg.Postgres.DataDir, "conf.d", "20-liops-monitor.conf"))
	if err != nil || !strings.Contains(string(conf), "shared_preload_libraries = 'timescaledb,pg_stat_statements'") {
		t.Errorf("conf: %s %v", conf, err)
	}
	all := strings.Join(sqls, "\n")
	for _, want := range []string{`CREATE ROLE "liops-monitor" LOGIN`, `GRANT pg_monitor TO "liops-monitor"`, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}

	// Already loaded: nothing to write, no restart.
	os.Remove(filepath.Join(cfg.Postgres.DataDir, "conf.d", "20-liops-monitor.conf"))
	libs, reloaded = "pg_stat_statements", false
	fake.Handle = func(_ string, c command.Cmd) ([]byte, error) {
		sql := c.Args[len(c.Args)-1]
		switch {
		case strings.Contains(sql, "current_setting('shared_preload_libraries')"):
			return []byte("libs:" + libs + "\n"), nil
		case strings.Contains(sql, "pg_reload_conf"):
			reloaded = true
		case strings.Contains(sql, "pending_restart"):
			return []byte("f\t0\n"), nil
		}
		return nil, nil
	}
	if restart, err := PostgresMonitorAccess(context.Background(), c); err != nil || restart || reloaded {
		t.Errorf("already loaded: restart %v, reloaded %v, err %v", restart, reloaded, err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Postgres.DataDir, "conf.d", "20-liops-monitor.conf")); err == nil {
		t.Error("nothing should be written when the library is loaded")
	}
}

func TestMySQLMonitorAccess(t *testing.T) {
	cfg := mysqlSetupConfig(t)
	var script string
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		last := c.Args[len(c.Args)-1]
		switch {
		case strings.Contains(last, "VERSION()"):
			return []byte("8.4.11\n"), nil
		case strings.Contains(last, "PLUGINS"):
			return []byte("1\n"), nil
		case strings.HasPrefix(last, "--execute=source "):
			data, err := os.ReadFile(strings.TrimPrefix(last, "--execute=source "))
			script = string(data)
			return nil, err
		}
		return nil, nil
	}}
	inst := mysql.Instance{Cfg: cfg.MySQL, Run: fake}
	if err := MySQLMonitorAccess(context.Background(), inst, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CREATE USER IF NOT EXISTS 'liops-monitor'@'localhost' IDENTIFIED WITH auth_socket",
		"GRANT PROCESS, REFERENCES ON *.* TO 'liops-monitor'@'localhost'", "GRANT SELECT ON performance_schema.*"} {
		if !strings.Contains(script, want) {
			t.Errorf("missing %q in:\n%s", want, script)
		}
	}
	if strings.Contains(script, "INSTALL PLUGIN") || strings.Contains(script, "SELECT ON *.*") {
		t.Errorf("auth_socket is loaded, and the account must not read rows:\n%s", script)
	}
}
