// SPDX-License-Identifier: Apache-2.0

package mysqldump

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

func mysqlConfig() *config.Config {
	cfg := config.Default()
	cfg.Engine = config.EngineMySQL
	cfg.MySQL.BackupName = "shop-db-1"
	cfg.Dumps.Exclude = []string{"scratch"}
	return cfg
}

func hexRows(names ...string) []byte {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(strings.ToUpper(hex.EncodeToString([]byte(n))) + "\n")
	}
	return []byte(b.String())
}

func TestDiscoverSkipsSystemSchemasAndExcludes(t *testing.T) {
	fake := &command.Fake{Handle: func(string, command.Cmd) ([]byte, error) {
		return hexRows("app", "information_schema", "mysql", "performance_schema", "scratch", "sys", "tenant-abc"), nil
	}}
	dbs, err := New(mysqlConfig(), fake, storage.NewMemory()).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dbs, []string{"app", "tenant-abc"}) {
		t.Errorf("Discover = %q", dbs)
	}
}

func TestDumpIsGzippedAndStreamed(t *testing.T) {
	var args []string
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		args = c.Args
		return []byte("-- MySQL dump\nCREATE TABLE t (id int);\n"), nil
	}}
	store := storage.NewMemory()
	at := time.Date(2026, 10, 28, 20, 0, 0, 0, time.UTC)
	key, size, err := New(mysqlConfig(), fake, store).Dump(context.Background(), "-odd", at)
	if err != nil {
		t.Fatal(err)
	}
	if key != "dumps/-odd/2026-10-28T200000Z.sql.gz" {
		t.Errorf("key = %q", key)
	}
	data, ok := store.Get(key)
	if !ok || size != int64(len(data)) {
		t.Fatalf("stored %d bytes (ok=%v), size %d", len(data), ok, size)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if !strings.HasPrefix(string(plain), "-- MySQL dump") {
		t.Errorf("gunzipped dump = %q", plain)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--single-transaction", "--routines", "--set-gtid-purged=OFF", "-- -odd"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mysqldump args %q are missing %q", joined, want)
		}
	}
}

func TestFailedDumpLeavesNoObject(t *testing.T) {
	fake := &command.Fake{Handle: func(string, command.Cmd) ([]byte, error) {
		return []byte("partial"), errors.New("mysqldump: Got error: 1045: Access denied")
	}}
	store := storage.NewMemory()
	if _, _, err := New(mysqlConfig(), fake, store).Dump(context.Background(), "app", time.Now()); err == nil || !strings.Contains(err.Error(), "Access denied") {
		t.Fatalf("expected the mysqldump error, got %v", err)
	}
	if objs, _ := store.List(context.Background(), ""); len(objs) != 0 {
		t.Errorf("a failed dump must not leave objects: %v", objs)
	}
}

func TestUsersSQL(t *testing.T) {
	users := strings.Join([]string{
		hex.EncodeToString([]byte("app")) + "\t" + hex.EncodeToString([]byte("%")),
		hex.EncodeToString([]byte("mysql.sys")) + "\t" + hex.EncodeToString([]byte("localhost")),
	}, "\n") + "\n"
	var queries []string
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		q := c.Args[len(c.Args)-1]
		queries = append(queries, q)
		if strings.Contains(q, "FROM mysql.user") {
			return []byte(users), nil
		}
		return []byte("CREATE USER `app`@`%` IDENTIFIED WITH 'caching_sha2_password' AS 0x2441 DEFAULT ROLE `app_reader`@`%` REQUIRE NONE\n" +
			"GRANT USAGE ON *.* TO `app`@`%`\nGRANT ALL PRIVILEGES ON `app`.* TO `app`@`%`\n"), nil
	}}
	sql, err := New(mysqlConfig(), fake, storage.NewMemory()).UsersSQL(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 || !strings.Contains(queries[1], "print_identified_with_as_hex = ON") || !strings.Contains(queries[1], "SHOW GRANTS FOR 'app'@'%'") {
		t.Errorf("mysql.sys must be skipped and hashes printed as hex: %q", queries)
	}
	create := strings.Index(sql, "CREATE USER IF NOT EXISTS `app`@`%`")
	grant := strings.Index(sql, "GRANT ALL PRIVILEGES ON `app`.* TO `app`@`%`;")
	if create < 0 || grant < create {
		t.Errorf("want CREATE USER IF NOT EXISTS before the grants, got:\n%s", sql)
	}
	// The role may be created after this user, so its default role is set at the end.
	setRole := strings.Index(sql, "SET DEFAULT ROLE `app_reader`@`%` TO 'app'@'%';")
	if strings.Contains(sql, "DEFAULT ROLE `app_reader`@`%` REQUIRE") || setRole < grant {
		t.Errorf("want the default role moved after the grants, got:\n%s", sql)
	}
}
