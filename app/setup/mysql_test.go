// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// fakeMySQL answers like a MySQL 8.4 server where root logs in through the socket when
// rootOK, and liops can log in once its defaults file exists.
type fakeMySQL struct {
	rootOK    bool
	rootPass  string
	version   string
	createSQL string
}

func (f *fakeMySQL) handle(_ string, c command.Cmd) ([]byte, error) {
	login := strings.TrimPrefix(c.Args[0], "--defaults-extra-file=")
	data, err := os.ReadFile(login)
	if err != nil {
		return nil, errors.New("mysql: Could not open required defaults file")
	}
	isRoot := strings.Contains(string(data), "user=root")
	if isRoot && !f.rootOK && !strings.Contains(string(data), f.rootPass) {
		return nil, errors.New("ERROR 1698 (28000): Access denied for user 'root'@'localhost'")
	}
	last := c.Args[len(c.Args)-1]
	switch {
	case strings.HasPrefix(last, "--execute=source "):
		sql, _ := os.ReadFile(strings.TrimPrefix(last, "--execute=source "))
		f.createSQL = string(sql)
		return nil, nil
	case strings.Contains(last, "VERSION()"):
		return []byte(f.version + "\n"), nil
	case strings.Contains(last, "@@server_uuid"):
		return []byte("3e11fa47-71ca-11e1-9e33-c80aa9429562\n"), nil
	}
	return nil, errors.New("unexpected query " + last)
}

func mysqlSetupConfig(t *testing.T) *config.Config {
	cfg := config.Default()
	cfg.Engine = config.EngineMySQL
	cfg.MySQL.BackupName = "shop-db-1"
	cfg.MySQL.DefaultsFile = filepath.Join(t.TempDir(), "liops", "mysql.cnf")
	return cfg
}

func TestSetupMySQLCreatesAccount(t *testing.T) {
	cfg := mysqlSetupConfig(t)
	f := &fakeMySQL{rootOK: true, version: "8.4.3"}
	store := storage.NewMemory()
	version, err := SetupMySQL(context.Background(), cfg, &command.Fake{Handle: f.handle}, store, "")
	if err != nil {
		t.Fatal(err)
	}
	if version != "8.4.3" {
		t.Errorf("version = %q", version)
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
	if _, err := SetupMySQL(context.Background(), cfg, &command.Fake{Handle: f.handle}, store, ""); err != nil || f.createSQL != "" {
		t.Errorf("second setup: %v, created again: %v", err, f.createSQL != "")
	}
}

func TestSetupMySQLAsksForRootPassword(t *testing.T) {
	cfg := mysqlSetupConfig(t)
	f := &fakeMySQL{rootPass: "s3cret#1", version: "8.4.3"}
	run := &command.Fake{Handle: f.handle}
	if _, err := SetupMySQL(context.Background(), cfg, run, storage.NewMemory(), ""); !errors.Is(err, ErrNeedRootPassword) {
		t.Fatalf("want ErrNeedRootPassword, got %v", err)
	}
	if _, err := SetupMySQL(context.Background(), cfg, run, storage.NewMemory(), "s3cret#1"); err != nil {
		t.Fatalf("with the root password: %v", err)
	}
}

func TestSetupMySQLRefusesOldVersion(t *testing.T) {
	f := &fakeMySQL{rootOK: true, version: "8.0.39"}
	_, err := SetupMySQL(context.Background(), mysqlSetupConfig(t), &command.Fake{Handle: f.handle}, storage.NewMemory(), "")
	if err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("want a too-old error, got %v", err)
	}
}

func TestSetupMySQLRefusesAnotherServersName(t *testing.T) {
	f := &fakeMySQL{rootOK: true, version: "8.4.3"}
	for name, existing := range map[string]string{
		"mysql":    "owners/mysql-another-uuid",
		"postgres": "pgbackrest/backup/shop-db-1/backup.info",
	} {
		store := storage.NewMemory()
		store.Add(existing, []byte("x"), time.Now())
		var taken *MySQLNameTakenError
		if _, err := SetupMySQL(context.Background(), mysqlSetupConfig(t), &command.Fake{Handle: f.handle}, store, ""); !errors.As(err, &taken) {
			t.Errorf("%s: want MySQLNameTakenError, got %v", name, err)
		}
	}
}
