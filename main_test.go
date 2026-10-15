// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestUserCommands(t *testing.T) {
	asRoot(t)
	statePath := filepath.Join(t.TempDir(), "state.db")
	var out bytes.Buffer
	if err := run([]string{"user", "add", "alice", "--state", statePath}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "alice's password: ") {
		t.Errorf("add should print the password: %q", out.String())
	}
	if err := run([]string{"user", "add", "--state", statePath, "alice"}, &out); err == nil {
		t.Error("adding alice twice should fail")
	}
	out.Reset()
	if err := run([]string{"user", "list", "--state", statePath}, &out); err != nil || !strings.Contains(out.String(), "alice") {
		t.Errorf("list: %v %q", err, out.String())
	}
	if err := run([]string{"user", "reset", "bob", "--state", statePath}, &out); err == nil {
		t.Error("resetting a missing user should fail")
	}
	if err := run([]string{"user", "delete", "alice", "--state", statePath}, &out); err != nil {
		t.Fatal(err)
	}
}

func TestSetupTokenCommand(t *testing.T) {
	asRoot(t)
	statePath := filepath.Join(t.TempDir(), "state.db")
	var a, b, c bytes.Buffer
	if err := run([]string{"setup-token", "--state", statePath}, &a); err != nil {
		t.Fatal(err)
	}
	run([]string{"setup-token", "--state", statePath}, &b)
	if a.String() != b.String() || len(strings.TrimSpace(a.String())) != 27 {
		t.Errorf("the token should be stable until --new: %q %q", a.String(), b.String())
	}
	run([]string{"setup-token", "--new", "--state", statePath}, &c)
	if c.String() == a.String() {
		t.Error("--new should make a new token")
	}
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
