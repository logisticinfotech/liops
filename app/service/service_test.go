// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/jobs"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/config"
)

func TestPicksUpSetupFinishedOnTheCommandLine(t *testing.T) {
	old := setupPoll
	setupPoll = 10 * time.Millisecond
	t.Cleanup(func() { setupPoll = old })

	dir := t.TempDir()
	cfg := config.Default()
	cfg.PgBackRest.Stanza = "main"
	cfg.Storage.S3.Endpoint, cfg.Storage.S3.Bucket = "example-account.r2.cloudflarestorage.com", "example-bucket"
	cfg.Storage.S3.AccessKeyID, cfg.Storage.S3.SecretAccessKey = "key", "secret"
	cfg.Email.Username, cfg.Email.Password, cfg.Email.From = "alerts@example.com", "app-password", "alerts@example.com"
	cfg.Email.To = []string{"dev@example.com"}
	configPath := filepath.Join(dir, "config.yaml")
	if err := cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	newRunner := func(cfg *config.Config) (*jobs.Runner, error) {
		return &jobs.Runner{Cfg: cfg, State: st, LockPath: filepath.Join(dir, "job.lock"), Now: time.Now, Log: io.Discard}, nil
	}
	svc, err := New(ctx, configPath, filepath.Join(dir, "job.lock"), st, io.Discard, newRunner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); svc.Close() })

	// install.sh started the service before setup: no schedule yet.
	if next := svc.Next(); len(next) != 0 {
		t.Fatalf("before setup, nothing should be scheduled: %v", next)
	}
	// `liops setup` finishes in another process.
	if err := st.SetMeta(MetaSetupComplete, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	for range 200 {
		if len(svc.Next()) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the service should start its schedule once setup is complete, without a restart")
}
