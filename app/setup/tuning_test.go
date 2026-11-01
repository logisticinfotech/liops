// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysql"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/tuning"
	"github.com/logisticinfotech/liops/config"
)

func TestTuningRefusesWhatItMustNotTouch(t *testing.T) {
	env := TuningEnv{Run: &command.Fake{}}
	for _, c := range []struct {
		kind string
		vals map[string]string
	}{
		{tuning.KindMySQL, map[string]string{"innodb_flush_log_at_trx_commit": "2"}},
		{tuning.KindPostgres, map[string]string{"fsync": "off"}},
		{tuning.KindPostgres, map[string]string{"synchronous_commit": "off"}},
		{tuning.KindMySQL, map[string]string{"max_connections": "1; DROP USER root"}},
		{tuning.KindRedis, map[string]string{"requirepass": "x"}},
	} {
		if _, _, err := ApplyTuning(context.Background(), env, c.kind, "", c.vals); err == nil {
			t.Errorf("%v should be refused", c.vals)
		}
	}
}

func TestMySQLTuning(t *testing.T) {
	cfg := mysqlSetupConfig(t)
	conf := filepath.Join(t.TempDir(), "zz-liops-tuning.cnf")
	var sets []string
	validate := error(nil)
	fake := &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
		last := c.Args[len(c.Args)-1]
		switch {
		case p == "mysqld":
			return []byte("mysqld: [ERROR] unknown variable"), validate
		case strings.Contains(last, "VERSION()"):
			return []byte("8.4.11\n"), nil
		case strings.Contains(last, "@@GLOBAL.innodb_buffer_pool_size"):
			return []byte("134217728\n"), nil
		case strings.Contains(last, "SET GLOBAL"):
			sets = append(sets, strings.TrimPrefix(last, "--execute="))
		}
		return nil, nil
	}}
	inst := mysql.Instance{Cfg: cfg.MySQL, Run: fake}
	env := TuningEnv{Run: fake, MySQL: &inst, MySQLConf: conf}
	old, restart, err := ApplyTuning(context.Background(), env, tuning.KindMySQL, "", map[string]string{"innodb_buffer_pool_size": "3840M"})
	if err != nil || restart || old["innodb_buffer_pool_size"] != "134217728" {
		t.Fatalf("old %v restart %v err %v", old, restart, err)
	}
	if data, _ := os.ReadFile(conf); !strings.Contains(string(data), "[mysqld]\ninnodb_buffer_pool_size = 3840M\n") {
		t.Errorf("conf:\n%s", data)
	}
	if len(sets) != 1 || sets[0] != "SET GLOBAL innodb_buffer_pool_size = 4026531840" {
		t.Errorf("online: %q", sets)
	}
	// A failed check puts the file back as it was.
	validate = errors.New("exit status 1")
	if _, _, err := ApplyTuning(context.Background(), env, tuning.KindMySQL, "", map[string]string{"max_connections": "300"}); err == nil {
		t.Fatal("a failed check should stop it")
	}
	if data, _ := os.ReadFile(conf); strings.Contains(string(data), "max_connections") {
		t.Errorf("the file should be put back:\n%s", data)
	}
}

func TestPostgresTuning(t *testing.T) {
	cfg := config.Default()
	cfg.Postgres.Layout, cfg.Postgres.DataDir = config.LayoutHomebrew, t.TempDir()
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		sql := c.Args[len(c.Args)-1]
		switch {
		case strings.HasPrefix(sql, "SHOW shared_buffers"):
			return []byte("128MB\n"), nil
		case strings.HasPrefix(sql, "SHOW random_page_cost"):
			return []byte("4\n"), nil
		case strings.Contains(sql, "pg_file_settings"):
			return []byte("\n"), nil
		case strings.Contains(sql, "pending_restart FROM pg_settings WHERE name = 'shared_buffers'"):
			return []byte("t\n"), nil
		case strings.Contains(sql, "pending_restart"):
			return []byte("f\n"), nil
		}
		return []byte("t\n"), nil
	}}
	c := postgres.Cluster{Cfg: cfg.Postgres, Run: fake}
	env := TuningEnv{Run: fake, Postgres: &c}
	old, restart, err := ApplyTuning(context.Background(), env, tuning.KindPostgres, "", map[string]string{"random_page_cost": "1.1"})
	if err != nil || restart || old["random_page_cost"] != "4" {
		t.Fatalf("old %v restart %v err %v", old, restart, err)
	}
	_, restart, err = ApplyTuning(context.Background(), env, tuning.KindPostgres, "", map[string]string{"shared_buffers": "4G"})
	if err != nil || !restart {
		t.Errorf("shared_buffers needs a restart: %v %v", restart, err)
	}
	data, _ := os.ReadFile(filepath.Join(cfg.Postgres.DataDir, "conf.d", "30-liops-tuning.conf"))
	if !strings.Contains(string(data), "random_page_cost = '1.1'\nshared_buffers = '4G'\n") {
		t.Errorf("conf:\n%s", data)
	}
}

func TestPHPAndPoolTuning(t *testing.T) {
	etc := t.TempDir()
	dir := filepath.Join(etc, "8.3", "fpm", "pool.d")
	os.MkdirAll(dir, 0o755)
	pool := "[www]\nlisten = /run/php/php8.3-fpm.sock\npm = dynamic\npm.max_children = 5 ; the default\npm.start_servers = 2\n\n[api]\npm.max_children = 3\n"
	os.WriteFile(filepath.Join(dir, "www.conf"), []byte(pool), 0o644)
	var calls []string
	fake := &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
		calls = append(calls, p+" "+strings.Join(c.Args, " "))
		return nil, nil
	}}
	env := TuningEnv{Run: fake, PHPEtc: etc, BackupDir: t.TempDir()}
	old, _, err := ApplyTuning(context.Background(), env, tuning.KindPool, "8.3/www", map[string]string{"pm.max_children": "14", "pm.max_spare_servers": "4"})
	if err != nil || old["pm.max_children"] != "5" || old["pm.max_spare_servers"] != "" {
		t.Fatalf("old %v err %v", old, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "www.conf"))
	if !strings.Contains(string(data), "pm.max_children = 14 ; liops tuning\npm.start_servers = 2\n\npm.max_spare_servers = 4 ; liops tuning\n[api]\npm.max_children = 3\n") {
		t.Errorf("pool:\n%s", data)
	}
	// Revert: the old values back, the added line gone.
	if _, _, err := ApplyTuning(context.Background(), env, tuning.KindPool, "8.3/www", old); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "www.conf"))
	if strings.Contains(string(data), "max_spare") || !strings.Contains(string(data), "pm.max_children = 5 ; liops tuning") {
		t.Errorf("reverted pool:\n%s", data)
	}

	old, _, err = ApplyTuning(context.Background(), env, tuning.KindPHP, "8.3", map[string]string{"opcache.memory_consumption": "256"})
	if err != nil || old["opcache.memory_consumption"] != "" {
		t.Fatalf("php: %v %v", old, err)
	}
	ini, _ := os.ReadFile(filepath.Join(etc, "8.3", "fpm", "conf.d", "zz-liops-tuning.ini"))
	if !strings.Contains(string(ini), "opcache.memory_consumption = 256\n") {
		t.Errorf("ini:\n%s", ini)
	}
	if !strings.Contains(strings.Join(calls, "|"), "php-fpm8.3 -t|systemctl reload php8.3-fpm") {
		t.Errorf("calls = %q", calls)
	}
}

func TestRedisTuning(t *testing.T) {
	var in string
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		b := new(strings.Builder)
		buf := make([]byte, 1024)
		n, _ := c.Stdin.Read(buf)
		b.Write(buf[:n])
		in = b.String()
		return []byte("maxmemory\n268435456\nOK\nOK\n"), nil
	}}
	old, _, err := ApplyTuning(context.Background(), TuningEnv{Run: fake, RedisAddr: "127.0.0.1:6379"}, tuning.KindRedis, "", map[string]string{"maxmemory": "384M"})
	if err != nil || old["maxmemory"] != "268435456" || !strings.Contains(in, "CONFIG SET maxmemory 384M\nCONFIG REWRITE") {
		t.Errorf("old %v err %v in %q", old, err, in)
	}
}
