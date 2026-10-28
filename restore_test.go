// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/config"
)

func TestParseMoment(t *testing.T) {
	cfg := config.Default() // Asia/Kolkata
	got, err := parseMoment("2026-10-28 14:05", cfg)
	if err != nil || !got.Equal(time.Date(2026, 10, 28, 8, 35, 0, 0, time.UTC)) {
		t.Errorf("parseMoment = %v, %v (want 14:05 in server.timezone)", got, err)
	}
	if got, err := parseMoment("", cfg); err != nil || !got.IsZero() {
		t.Errorf("empty means as late as possible: %v, %v", got, err)
	}
	if _, err := parseMoment("yesterday", cfg); err == nil || !strings.Contains(err.Error(), "2026-10-28 14:05") {
		t.Errorf("want an example in the error, got %v", err)
	}
}

func TestEmptyRestoreDir(t *testing.T) {
	base := t.TempDir()
	if err := emptyRestoreDir(filepath.Join(base, "new", "restore"), "/var/lib/mysql"); err != nil {
		t.Errorf("a directory that doesn't exist yet is created: %v", err)
	}
	full := filepath.Join(base, "full")
	os.MkdirAll(full, 0o700)
	os.WriteFile(filepath.Join(full, "x"), nil, 0o600)
	for dir, want := range map[string]string{
		full:               "isn't empty",
		"restore":          "absolute path",
		"/var/lib/mysql/r": "outside /var/lib/mysql",
		"/var/lib/mysql":   "outside /var/lib/mysql",
	} {
		if err := emptyRestoreDir(dir, "/var/lib/mysql"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", dir, want, err)
		}
	}
}
