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

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/storage"
	"github.com/logisticinfotech/pgtijori/config"
)

const testStanza = "main"

func TestKeyRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 5, 1, 31, 2, 0, time.FixedZone("IST", 5*3600+1800))
	for _, db := range []string{"app", "tenant_abc", "odd/name", "with space", "100%"} {
		key := Key(testStanza, db, at)
		if !strings.HasPrefix(key, "pgbackrest/backup/main/dumps/") {
			t.Errorf("key %q should be under the stanza dumps prefix", key)
		}
		gotDB, gotT, ok := ParseKey(key)
		if !ok || gotDB != db || !gotT.Equal(at) {
			t.Errorf("ParseKey(%q) = %q, %v, %v", key, gotDB, gotT, ok)
		}
	}
	if got := Key(testStanza, "app", at); got != "pgbackrest/backup/main/dumps/app/2026-10-04T200102Z.dump" {
		t.Errorf("keys use UTC: got %q", got)
	}
	// Legacy layout still parses.
	if db, _, ok := ParseKey("dumps/app/2026-10-04T200102Z.dump"); !ok || db != "app" {
		t.Errorf("legacy ParseKey failed: %q %v", db, ok)
	}
	for _, bad := range []string{"dumps/app", "pgbackrest/x/2026-10-04T200102Z.dump", "dumps/app/notatime.dump", "dumps/a/b/2026-10-04T200102Z.dump"} {
		if _, _, ok := ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) should fail", bad)
		}
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
	cfg := config.Default()
	cfg.PgBackRest.Stanza = testStanza
	e := New(cfg, fake, store)
	at := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)

	key, size, err := e.Dump(context.Background(), "a=b", at)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "pgbackrest/backup/main/dumps/") {
		t.Errorf("dump key = %q", key)
	}
	if data, ok := store.Bytes(key); !ok || string(data) != "PGDMP-data" || size != int64(len(data)) {
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
	cfg := config.Default()
	cfg.PgBackRest.Stanza = testStanza
	e := New(cfg, fake, store)
	if _, _, err := e.Dump(context.Background(), "app", time.Now()); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("expected the pg_dump error, got %v", err)
	}
	if objs, _ := store.List(context.Background(), ""); len(objs) != 0 {
		t.Errorf("a failed dump must not leave objects: %v", objs)
	}
}

func TestFailedUploadIsReported(t *testing.T) {
	store := storage.NewMemory()
	store.FailPut = "pgbackrest/backup/main/dumps/"
	cfg := config.Default()
	cfg.PgBackRest.Stanza = testStanza
	e := New(cfg, &command.Fake{Handle: func(string, command.Cmd) ([]byte, error) { return []byte("x"), nil }}, store)
	if _, _, err := e.Dump(context.Background(), "app", time.Now()); err == nil {
		t.Fatal("expected the upload error")
	}
}

func TestLabels(t *testing.T) {
	cfg := config.Default()
	cfg.PgBackRest.Stanza = testStanza
	cfg.Dumps.Labels = config.LabelQuery{Database: "central", SQL: "SELECT db_name, subdomain FROM tenants"}
	fake := &command.Fake{Handle: func(string, command.Cmd) ([]byte, error) {
		return []byte("tenant_abc\tabc\ntenant_xyz\txyz\n"), nil
	}}
	labels, err := New(cfg, fake, storage.NewMemory()).Labels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if labels["tenant_abc"] != "abc" || labels["tenant_xyz"] != "xyz" {
		t.Errorf("labels = %v", labels)
	}
}
