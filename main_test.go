// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/logisticinfotech/pgtijori/app/service"
	"github.com/logisticinfotech/pgtijori/app/state"
)

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "pgtijori ") {
		t.Errorf("unexpected version output: %q", out.String())
	}
}

func TestConfigInitThenValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	var out bytes.Buffer

	if err := run([]string{"config", "init", "--config", path}, &out); err != nil {
		t.Fatalf("init: %v", err)
	}
	// A second init must not overwrite the file.
	if err := run([]string{"config", "init", "--config", path}, &out); err == nil {
		t.Fatal("second init without --force should fail")
	}
	// The fresh file is missing storage/email settings, so validate must list them.
	err := run([]string{"config", "validate", "--config", path}, &out)
	if err == nil || !strings.Contains(err.Error(), "storage.s3.bucket") {
		t.Fatalf("validate should report missing settings, got: %v", err)
	}
}

func TestUnknownCommand(t *testing.T) {
	if err := run([]string{"frobnicate"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected an error for an unknown command")
	}
}

func asRoot(t *testing.T) {
	old := requireRoot
	requireRoot = func(string) error { return nil }
	t.Cleanup(func() { requireRoot = old })
}

func TestLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pgtijori.log")
	os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600)
	var out bytes.Buffer
	if err := run([]string{"logs", "-n", "2", "-log", path}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "two\nthree\n" {
		t.Errorf("logs = %q", out.String())
	}
}

func TestDevModeDefaults(t *testing.T) {
	t.Setenv(devEnv, "")
	if p := defaultPaths(); p.config != "/etc/pgtijori/config.yaml" {
		t.Errorf("production config path = %q", p.config)
	}
	t.Setenv(devEnv, "1")
	if p := defaultPaths(); p.config != "dev/config.yaml" || p.state != "dev/state.db" || p.log != "dev/pgtijori.log" {
		t.Errorf("dev paths = %+v", p)
	}
	if err := requireRoot("test"); err != nil {
		t.Errorf("dev mode should not need root: %v", err)
	}
}

const validConfig = `pgbackrest: {stanza: dev}
storage: {s3: {endpoint: localhost:9000, bucket: example-bucket, access_key_id: key, secret_access_key: secret}}
email: {smtp_host: localhost, username: dev, password: dev, from: alerts@example.com, to: [dev@example.com]}
`

func TestSetupSkipPgBackRest(t *testing.T) {
	asRoot(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"setup", "--skip-pgbackrest", "--config", cfg, "--state", filepath.Join(dir, "state.db")}

	t.Setenv(devEnv, "")
	if err := run(args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "only for development") {
		t.Fatalf("--skip-pgbackrest outside dev mode should be refused, got: %v", err)
	}

	t.Setenv(devEnv, "1")
	var out bytes.Buffer
	if err := run(args, &out); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := st.GetMeta(service.MetaSetupComplete); err != nil || v == "" {
		t.Errorf("setup should be marked complete: %q, %v", v, err)
	}
}
