// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// complete returns a valid config with example (non-real) values.
func complete() *Config {
	c := Default()
	c.PgBackRest.Stanza = "main"
	c.Storage.S3.Endpoint = "example-account.r2.cloudflarestorage.com"
	c.Storage.S3.Bucket = "example-pg-backups"
	c.Storage.S3.AccessKeyID = "key"
	c.Storage.S3.SecretAccessKey = "secret"
	c.Email.Username = "alerts@example.com"
	c.Email.Password = "app-password"
	c.Email.From = "alerts@example.com"
	c.Email.To = []string{"dev@example.com"}
	return c
}

func TestDefaultIsValidOnceCompleted(t *testing.T) {
	if err := complete().Validate(); err != nil {
		t.Fatalf("completed default config should be valid, got: %v", err)
	}
}

func TestDefaultReportsMissingSettings(t *testing.T) {
	err := Default().Validate()
	if err == nil {
		t.Fatal("bare default config should not be valid")
	}
	for _, want := range []string{"pgbackrest.stanza", "storage.s3.endpoint", "storage.s3.bucket", "email.to"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q, got:\n%v", want, err)
		}
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "liops", "config.yaml")
	want := complete()
	want.PgBackRest.ArchiveTimeout = Duration(15 * time.Minute)

	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("saved file mode = %04o, want 0600", perm)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.PgBackRest.ArchiveTimeout != want.PgBackRest.ArchiveTimeout {
		t.Errorf("archive_timeout = %v, want %v", got.PgBackRest.ArchiveTimeout, want.PgBackRest.ArchiveTimeout)
	}
	if got.Storage.S3.Bucket != want.Storage.S3.Bucket || got.Email.To[0] != want.Email.To[0] {
		t.Errorf("round trip lost values: %+v", got)
	}
}

func TestLoadRefusesReadableFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := complete().Save(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("expected a permissions error, got: %v", err)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte("pgbackrest:\n  stanzaa: main\n"))
	if err == nil || !strings.Contains(err.Error(), "stanzaa") {
		t.Fatalf("expected unknown-key error, got: %v", err)
	}
}

func TestParseEmptyFileUsesDefaults(t *testing.T) {
	// An empty file decodes to the defaults, which then fail validation for the
	// missing required settings (not with a YAML error).
	_, err := Parse(nil)
	if err == nil || !strings.Contains(err.Error(), "pgbackrest.stanza") {
		t.Fatalf("expected validation error for missing settings, got: %v", err)
	}
}

func TestUIMustListenOnLoopback(t *testing.T) {
	for listen, ok := range map[string]bool{
		"127.0.0.1:8899": true,
		"[::1]:8899":     true,
		"0.0.0.0:8899":   false,
		"10.0.0.5:8899":  false,
		"localhost:8899": false, // must be an IP, so it can't resolve to something else
		":8899":          false,
		"127.0.0.1":      false,
	} {
		c := complete()
		c.UI.Listen = listen
		err := c.Validate()
		if ok && err != nil {
			t.Errorf("%q should be accepted, got: %v", listen, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), "ui.listen")) {
			t.Errorf("%q should be rejected, got: %v", listen, err)
		}
	}
}

func TestValidateChecksSchedulesAndValues(t *testing.T) {
	c := complete()
	c.PgBackRest.FullSchedule = "every sunday"
	c.PgBackRest.ArchiveTimeout = Duration(10 * time.Second)
	c.Server.Timezone = "Mars/Olympus"
	c.Storage.S3.Endpoint = "https://example.r2.cloudflarestorage.com"
	c.Email.To = []string{"not-an-address"}
	c.Dumps.Labels.Database = "central" // sql missing
	c.Email.TLS = "none"
	c.PgBackRest.Stanza = "app/prod"
	c.Postgres.BinDir = "opt/pg/bin"
	c.Postgres.Layout = "windows"

	err := c.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"pgbackrest.full_schedule",
		"pgbackrest.archive_timeout",
		"server.timezone",
		"storage.s3.endpoint: give the host only",
		"email.to",
		"dumps.labels",
		"email.tls",
		"pgbackrest.stanza",
		"postgres.bin_dir",
		"postgres.layout",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to mention %q, got:\n%v", want, err)
		}
	}
}

func TestDumpSettingsIgnoredWhenDisabled(t *testing.T) {
	c := complete()
	c.Dumps.Enabled = false
	c.Dumps.Concurrency = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("dump settings should not be validated when dumps are disabled: %v", err)
	}
}

func TestLoadDraft(t *testing.T) {
	dir := t.TempDir()
	missing, err := LoadDraft(filepath.Join(dir, "missing.yaml"))
	if err != nil || missing.UI.Listen != "127.0.0.1:8899" {
		t.Fatalf("a missing file gives the defaults: %v %+v", err, missing)
	}
	path := filepath.Join(dir, "draft.yaml")
	if err := Default().Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDraft(path); err != nil {
		t.Errorf("an incomplete draft loads: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load must still validate")
	}
}

func TestClone(t *testing.T) {
	a := complete()
	b := a.Clone()
	b.Email.To[0] = "other@example.com"
	b.Dumps.Exclude = append(b.Dumps.Exclude, "x")
	if a.Email.To[0] != "dev@example.com" || len(a.Dumps.Exclude) != 1 {
		t.Error("Clone must not share slices")
	}
}

func TestMySQLEngine(t *testing.T) {
	c := complete()
	c.Engine = EngineMySQL
	c.PgBackRest.Stanza = "" // PostgreSQL settings aren't checked for a MySQL server
	c.Postgres.Version = 9
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "mysql.backup_name: required") {
		t.Fatalf("want a missing mysql.backup_name, got %v", err)
	}
	if strings.Contains(err.Error(), "postgres.") || strings.Contains(err.Error(), "pgbackrest.") {
		t.Errorf("PostgreSQL settings must not be validated for MySQL: %v", err)
	}

	c.MySQL.BackupName = "shop-db-1"
	if err := c.Validate(); err != nil {
		t.Fatalf("complete MySQL config should be valid, got: %v", err)
	}
	if got := c.BackupName(); got != "shop-db-1" || !c.IsMySQL() {
		t.Errorf("BackupName = %q, IsMySQL = %v", got, c.IsMySQL())
	}

	c.MySQL.Socket = "mysqld.sock"
	c.MySQL.FullSchedule = "nightly"
	err = c.Validate()
	for _, want := range []string{"mysql.socket", "mysql.full_schedule"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error about %s, got %v", want, err)
		}
	}
}

func TestUnknownEngine(t *testing.T) {
	c := complete()
	c.Engine = "oracle"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), `engine: "oracle"`) {
		t.Errorf("want an engine error, got %v", err)
	}
}
