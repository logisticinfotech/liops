// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// fakeMySQL answers like a MySQL 8.4 server where root logs in through the socket when
// rootOK, and liops can log in once its defaults file exists. Its GTID and binlog
// settings change when setup sets them, and only in the order MySQL allows.
type fakeMySQL struct {
	rootOK     bool
	rootPass   string
	version    string
	xtrabackup string // its version; empty if it isn't installed
	gtidMode   string
	enforce    string
	expiry     int
	replicas   bool

	createSQL string
	executed  []string // SET and RESET statements, in order
}

// readyMySQL is a server that already has everything point-in-time backups need.
func readyMySQL() *fakeMySQL {
	return &fakeMySQL{rootOK: true, version: "8.4.11-0ubuntu0.26.04.1", xtrabackup: "8.4.0-7", gtidMode: "ON", enforce: "ON", expiry: 2592000}
}

func (f *fakeMySQL) handle(program string, c command.Cmd) ([]byte, error) {
	if program == "xtrabackup" {
		if f.xtrabackup == "" {
			return nil, errors.New("exec: xtrabackup: not found")
		}
		return []byte("xtrabackup version " + f.xtrabackup + " based on MySQL server 8.4.0 Linux (aarch64) (revision id: 4093cfd8)\n"), nil
	}
	login := strings.TrimPrefix(c.Args[0], "--defaults-extra-file=")
	data, err := os.ReadFile(login)
	if err != nil {
		return nil, errors.New("mysql: Could not open required defaults file")
	}
	isRoot := strings.Contains(string(data), "user=root")
	if isRoot && !f.rootOK && !strings.Contains(string(data), f.rootPass) {
		return nil, errors.New("ERROR 1698 (28000): Access denied for user 'root'@'localhost'")
	}
	q := strings.TrimPrefix(c.Args[len(c.Args)-1], "--execute=")
	set := func(name string) (string, bool) {
		v, ok := strings.CutPrefix(q, "SET GLOBAL "+name+" = ")
		return v, ok
	}
	switch {
	case strings.HasPrefix(q, "source "):
		sql, _ := os.ReadFile(strings.TrimPrefix(q, "source "))
		f.createSQL = string(sql)
		return nil, nil
	case strings.Contains(q, "VERSION()"):
		return []byte(f.version + "\n"), nil
	case strings.Contains(q, "@@server_uuid"):
		return []byte("3e11fa47-71ca-11e1-9e33-c80aa9429562\n"), nil
	case strings.Contains(q, "@@log_bin"):
		return fmt.Appendf(nil, "1\tROW\t%s\t%s\t%d\n", f.gtidMode, f.enforce, f.expiry), nil
	case strings.Contains(q, "information_schema.TABLES"):
		return []byte("2\n"), nil
	case q == "SHOW REPLICA STATUS":
		return nil, nil
	case q == "SHOW REPLICAS":
		if f.replicas {
			return []byte("2\t\t3306\t1\tf0e1d2c3-0000-11e1-9e33-c80aa9429562\n"), nil
		}
		return nil, nil
	case strings.Contains(q, "Ongoing_anonymous_transaction_count"):
		return []byte("Ongoing_anonymous_transaction_count\t0\n"), nil
	}
	if !isRoot && (strings.HasPrefix(q, "SET ") || strings.HasPrefix(q, "RESET ")) {
		return nil, errors.New("ERROR 1227 (42000): Access denied; you need (at least one of) the SUPER or SYSTEM_VARIABLES_ADMIN privilege(s)")
	}
	f.executed = append(f.executed, q)
	if v, ok := set("gtid_mode"); ok {
		if slices.Index(gtidModes, v) != slices.Index(gtidModes, f.gtidMode)+1 {
			return nil, fmt.Errorf("ERROR 1788 (HY000): The value of @@GLOBAL.GTID_MODE can only be changed one step at a time (from %s to %s)", f.gtidMode, v)
		}
		if v == "ON" && f.enforce != "ON" {
			return nil, errors.New("ERROR 1782 (HY000): GTID_MODE = ON requires ENFORCE_GTID_CONSISTENCY = ON")
		}
		f.gtidMode = v
		return nil, nil
	}
	if v, ok := set("enforce_gtid_consistency"); ok {
		f.enforce = v
		return nil, nil
	}
	if v, ok := set("binlog_expire_logs_seconds"); ok {
		_, err := fmt.Sscan(v, &f.expiry)
		return nil, err
	}
	if strings.HasPrefix(q, "RESET PERSIST IF EXISTS ") {
		return nil, nil
	}
	return nil, errors.New("unexpected query " + q)
}

func mysqlSetupConfig(t *testing.T) *config.Config {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Engine = config.EngineMySQL
	cfg.MySQL.BackupName = "shop-db-1"
	cfg.MySQL.DefaultsFile = filepath.Join(dir, "liops", "mysql.cnf")
	cfg.MySQL.ConfFile = filepath.Join(dir, "zz-liops.cnf")
	return cfg
}

func setupMySQL(cfg *config.Config, f *fakeMySQL, store storage.Store, opts MySQLOptions) (MySQLResult, error) {
	if store == nil {
		store = storage.NewMemory()
	}
	return SetupMySQL(context.Background(), cfg, &command.Fake{Handle: f.handle}, store, opts)
}

func TestSetupMySQLCreatesAccount(t *testing.T) {
	cfg := mysqlSetupConfig(t)
	f := readyMySQL()
	store := storage.NewMemory()
	res, err := setupMySQL(cfg, f, store, MySQLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "8.4.11-0ubuntu0.26.04.1" || len(res.Pending) != 0 {
		t.Errorf("result = %+v", res)
	}
	if !strings.Contains(f.createSQL, "CREATE USER IF NOT EXISTS 'liops'@'localhost'") || !strings.Contains(f.createSQL, "BACKUP_ADMIN") {
		t.Errorf("create SQL = %q", f.createSQL)
	}
	info, err := os.Stat(cfg.MySQL.DefaultsFile)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("defaults file: %v, mode %v", err, info)
	}
	data, _ := os.ReadFile(cfg.MySQL.DefaultsFile)
	if !strings.Contains(string(data), "user=liops\npassword=") {
		t.Errorf("defaults file = %q", data)
	}
	if _, ok := store.Get("owners/mysql-3e11fa47-71ca-11e1-9e33-c80aa9429562"); !ok {
		t.Error("setup should record this server as the owner of the backup name")
	}

	// Again: the account works, so root isn't needed.
	f.rootOK, f.createSQL = false, ""
	if _, err := setupMySQL(cfg, f, store, MySQLOptions{}); err != nil || f.createSQL != "" {
		t.Errorf("second setup: %v, created again: %v", err, f.createSQL != "")
	}
}

func TestSetupMySQLWritesSettingsFile(t *testing.T) {
	cfg := mysqlSetupConfig(t)
	if _, err := setupMySQL(cfg, readyMySQL(), nil, MySQLOptions{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.MySQL.ConfFile)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("settings file: %v, mode %v", err, info)
	}
	data, _ := os.ReadFile(cfg.MySQL.ConfFile)
	for _, want := range []string{"[mysqld]\n", "gtid_mode = ON\n", "enforce_gtid_consistency = ON\n", "binlog_expire_logs_seconds = 2592000\n"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("settings file is missing %q:\n%s", want, data)
		}
	}

	cfg.MySQL.ConfFile = "/nonexistent-dir/zz-liops.cnf"
	if _, err := setupMySQL(cfg, readyMySQL(), nil, MySQLOptions{}); err == nil || !strings.Contains(err.Error(), "mysql.conf_file") {
		t.Errorf("a settings directory that doesn't exist should be reported, got %v", err)
	}
}

func TestSetupMySQLAsksBeforeChangingSettings(t *testing.T) {
	cfg := mysqlSetupConfig(t)
	f := readyMySQL()
	f.gtidMode, f.enforce, f.expiry = "OFF", "OFF", 86400
	res, err := setupMySQL(cfg, f, nil, MySQLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range res.Pending {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"enforce_gtid_consistency", "gtid_mode", "binlog_expire_logs_seconds"}) || res.NonInnoDBTables != 2 {
		t.Errorf("pending = %v, non-InnoDB tables = %d", res.Pending, res.NonInnoDBTables)
	}
	if len(f.executed) != 0 || f.gtidMode != "OFF" {
		t.Errorf("nothing may change without approval: ran %q", f.executed)
	}
	if _, err := os.Stat(cfg.MySQL.ConfFile); err == nil {
		t.Error("the settings file must not be written before the change is approved")
	}

	res, err = setupMySQL(cfg, f, nil, MySQLOptions{ApplySettings: true})
	if err != nil {
		t.Fatalf("apply: %v (ran %q)", err, f.executed)
	}
	if len(res.Pending) != 0 || f.gtidMode != "ON" || f.enforce != "ON" || f.expiry != 604800 {
		t.Errorf("after apply: %+v, gtid_mode %s, enforce %s, expiry %d", res, f.gtidMode, f.enforce, f.expiry)
	}
	want := []string{
		"RESET PERSIST IF EXISTS gtid_mode", "RESET PERSIST IF EXISTS enforce_gtid_consistency", "RESET PERSIST IF EXISTS binlog_expire_logs_seconds",
		"SET GLOBAL enforce_gtid_consistency = WARN", "SET GLOBAL enforce_gtid_consistency = ON",
		"SET GLOBAL gtid_mode = OFF_PERMISSIVE", "SET GLOBAL gtid_mode = ON_PERMISSIVE", "SET GLOBAL gtid_mode = ON",
		"SET GLOBAL binlog_expire_logs_seconds = 604800",
	}
	if !slices.Equal(f.executed, want) {
		t.Errorf("statements:\n got %q\nwant %q", f.executed, want)
	}
	if data, _ := os.ReadFile(cfg.MySQL.ConfFile); !strings.Contains(string(data), "binlog_expire_logs_seconds = 604800") {
		t.Errorf("settings file:\n%s", data)
	}
}

func TestSetupMySQLWontChangeGTIDsOnAReplicatingServer(t *testing.T) {
	f := readyMySQL()
	f.gtidMode, f.enforce, f.replicas = "OFF", "OFF", true
	_, err := setupMySQL(mysqlSetupConfig(t), f, nil, MySQLOptions{ApplySettings: true})
	if err == nil || !strings.Contains(err.Error(), "replicates") || f.gtidMode != "OFF" {
		t.Errorf("want a replication refusal with nothing changed, got %v (gtid_mode %s)", err, f.gtidMode)
	}
}

func TestSetupMySQLNeedsMatchingXtraBackup(t *testing.T) {
	f := readyMySQL()
	f.xtrabackup = ""
	if _, err := setupMySQL(mysqlSetupConfig(t), f, nil, MySQLOptions{}); !errors.Is(err, ErrNoXtraBackup) {
		t.Errorf("want ErrNoXtraBackup, got %v", err)
	}
	f.xtrabackup = "8.0.35-30"
	if _, err := setupMySQL(mysqlSetupConfig(t), f, nil, MySQLOptions{}); err == nil || !strings.Contains(err.Error(), "percona-xtrabackup-84") {
		t.Errorf("want an XtraBackup version error naming the right package, got %v", err)
	}
}

func TestSetupMySQLAsksForRootPassword(t *testing.T) {
	cfg := mysqlSetupConfig(t)
	f := readyMySQL()
	f.rootOK, f.rootPass = false, "s3cret#1"
	if _, err := setupMySQL(cfg, f, nil, MySQLOptions{}); !errors.Is(err, ErrNeedRootPassword) {
		t.Fatalf("want ErrNeedRootPassword, got %v", err)
	}
	if _, err := setupMySQL(cfg, f, nil, MySQLOptions{RootPassword: "s3cret#1"}); err != nil {
		t.Fatalf("with the root password: %v", err)
	}
}

func TestSetupMySQLRefusesOldVersion(t *testing.T) {
	f := readyMySQL()
	f.version = "8.0.39"
	if _, err := setupMySQL(mysqlSetupConfig(t), f, nil, MySQLOptions{}); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("want a too-old error, got %v", err)
	}
}

func TestSetupMySQLRefusesAnotherServersName(t *testing.T) {
	for name, existing := range map[string]string{
		"another MySQL server":       "owners/mysql-f0e1d2c3-0000-11e1-9e33-c80aa9429562",
		"a PostgreSQL server":        "owners/postgres-7683116411252273504",
		"PostgreSQL, before markers": "pgbackrest/backup/shop-db-1/backup.info",
		"dumps nobody claimed":       "dumps/app/2026-10-01T013000Z.sql.gz",
	} {
		store := storage.NewMemory()
		store.Add(existing, []byte("x"), time.Now())
		var taken *NameTakenError
		_, err := setupMySQL(mysqlSetupConfig(t), readyMySQL(), store, MySQLOptions{})
		if !errors.As(err, &taken) || !strings.Contains(err.Error(), "mysql.backup_name") || !strings.Contains(err.Error(), "restore-mysql.md") {
			t.Errorf("%s: want a NameTakenError for MySQL, got %v", name, err)
		}
		if _, ok := store.Get("owners/mysql-3e11fa47-71ca-11e1-9e33-c80aa9429562"); ok {
			t.Errorf("%s: a refused folder must not get this server's marker", name)
		}
	}
}
