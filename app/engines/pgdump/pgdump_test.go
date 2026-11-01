// SPDX-License-Identifier: Apache-2.0

package pgdump

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

func TestKeyUsesDumpExtension(t *testing.T) {
	if got := Key("app", time.Date(2026, 10, 4, 20, 1, 2, 0, time.UTC)); got != "dumps/app/2026-10-04T200102Z.dump" {
		t.Errorf("Key = %q", got)
	}
}

func hexRows(names ...string) []byte {
	var b strings.Builder
	for _, n := range names {
		b.WriteString(hex.EncodeToString([]byte(n)) + "\n")
	}
	return []byte(b.String())
}

func TestDiscoverDecodesAndExcludes(t *testing.T) {
	fake := &command.Fake{Handle: func(string, command.Cmd) ([]byte, error) {
		return hexRows("app", "postgres", "tenant\twith\ttabs"), nil
	}}
	e := New(config.Default(), fake, storage.NewMemory())
	dbs, err := e.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(dbs, []string{"app", "tenant\twith\ttabs"}) {
		t.Errorf("Discover = %q", dbs)
	}
	if c := fake.Calls[0]; c.User != "postgres" || c.Name != "/usr/lib/postgresql/18/bin/psql" {
		t.Errorf("psql should run as postgres from the cluster's bin dir: %+v", c)
	}
}

func TestDumpStreamsToStorage(t *testing.T) {
	var args []string
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		args = c.Args
		return []byte("PGDMP-data"), nil
	}}
	store := storage.NewMemory()
	e := New(config.Default(), fake, store)
	at := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)

	key, size, err := e.Dump(context.Background(), "a=b", at)
	if err != nil {
		t.Fatal(err)
	}
	if data, ok := store.Get(key); !ok || string(data) != "PGDMP-data" || size != int64(len(data)) {
		t.Errorf("stored %q (ok=%v), size %d", data, ok, size)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-Fc -Z zstd:3") || !strings.Contains(joined, "-d dbname='a=b'") {
		t.Errorf("unexpected pg_dump args: %q", joined)
	}
}

func TestFailedDumpLeavesNoObject(t *testing.T) {
	fake := &command.Fake{Handle: func(string, command.Cmd) ([]byte, error) {
		return []byte("partial"), errors.New("pg_dump: connection refused")
	}}
	store := storage.NewMemory()
	e := New(config.Default(), fake, store)
	if _, _, err := e.Dump(context.Background(), "app", time.Now()); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected the pg_dump error, got %v", err)
	}
	if objs, _ := store.List(context.Background(), ""); len(objs) != 0 {
		t.Errorf("a failed dump must not leave objects: %v", objs)
	}
}

func TestFailedUploadIsReported(t *testing.T) {
	store := storage.NewMemory()
	store.FailPut = "dumps/"
	e := New(config.Default(), &command.Fake{Handle: func(string, command.Cmd) ([]byte, error) { return []byte("x"), nil }}, store)
	if _, _, err := e.Dump(context.Background(), "app", time.Now()); err == nil {
		t.Fatal("expected the upload error")
	}
}

func TestLabels(t *testing.T) {
	cfg := config.Default()
	cfg.Dumps.Labels = config.LabelQuery{Database: "central", Table: "tenants", DBColumn: "db_name", LabelColumn: "subdomain"}
	var gotSQL string
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		gotSQL = c.Args[len(c.Args)-1]
		return []byte("tenant_abc\tabc\ntenant_xyz\txyz\n"), nil
	}}
	labels, err := New(cfg, fake, storage.NewMemory()).Labels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if labels["tenant_abc"] != "abc" || labels["tenant_xyz"] != "xyz" {
		t.Errorf("labels = %v", labels)
	}
	if gotSQL != `SELECT "db_name", "subdomain" FROM "tenants"` && !strings.Contains(gotSQL, `"db_name"`) {
		t.Errorf("query = %q", gotSQL)
	}

	cfg.Dumps.Labels.SQL = "SELECT pg_read_file('/etc/liops/config.yaml')"
	cfg.Dumps.Labels.Table, cfg.Dumps.Labels.DBColumn, cfg.Dumps.Labels.LabelColumn = "", "", ""
	if labels, err := New(cfg, fake, storage.NewMemory()).Labels(context.Background()); err != nil || labels != nil {
		t.Errorf("leftover sql must not run: %v, %v", labels, err)
	}
	cfg.Dumps.Labels = config.LabelQuery{Database: "central", Table: "tenants;SELECT pg_read_file('x')", DBColumn: "db_name", LabelColumn: "lo_export"}
	if _, err := New(cfg, fake, storage.NewMemory()).Labels(context.Background()); err == nil {
		t.Error("function-like identifiers must be refused")
	}
}
