// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/jobs"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
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

func completeCfg() *config.Config {
	cfg := config.Default()
	cfg.PgBackRest.Stanza = "main"
	cfg.Storage.S3.Endpoint, cfg.Storage.S3.Bucket = "example-account.r2.cloudflarestorage.com", "example-bucket"
	cfg.Storage.S3.AccessKeyID, cfg.Storage.S3.SecretAccessKey = "key", "secret"
	cfg.Email.Username, cfg.Email.Password, cfg.Email.From = "alerts@example.com", "app-password", "alerts@example.com"
	cfg.Email.To = []string{"dev@example.com"}
	return cfg
}

func TestUpdateSettingsMergesUnderLock(t *testing.T) {
	dir := t.TempDir()
	cfg := completeCfg()
	configPath := filepath.Join(dir, "config.yaml")
	if err := cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.SetMeta(MetaSetupComplete, "yes")
	ctx, cancel := context.WithCancel(context.Background())
	newRunner := func(cfg *config.Config) (*jobs.Runner, error) {
		return &jobs.Runner{Cfg: cfg, State: st, LockPath: filepath.Join(dir, "job.lock"), Now: time.Now, Log: io.Discard}, nil
	}
	svc, err := New(ctx, configPath, filepath.Join(dir, "job.lock"), st, io.Discard, newRunner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); svc.Close() })
	svc.WriteSettings = func(context.Context, *config.Config) (setup.Archiving, error) { return setup.Ready, nil }
	svc.SetMonitor = func(bool) error { return nil }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := svc.UpdateSettings(context.Background(), func(c *config.Config) error {
			time.Sleep(20 * time.Millisecond)
			c.Storage.S3.Bucket = "bucket-a"
			return nil
		}); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := svc.UpdateSettings(context.Background(), func(c *config.Config) error {
			time.Sleep(10 * time.Millisecond)
			c.Email.From = "other@example.com"
			return nil
		}); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	got := svc.Config()
	if got.Storage.S3.Bucket != "bucket-a" || got.Email.From != "other@example.com" {
		t.Errorf("both section saves should stick: bucket=%q from=%q", got.Storage.S3.Bucket, got.Email.From)
	}
}

type blockList struct {
	storage.Store
	started chan struct{}
	release chan struct{}
	n       atomic.Int32
}

func (b *blockList) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	if b.n.Add(1) == 1 {
		close(b.started)
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.Store.List(ctx, prefix)
}

func TestHealthSingleFlight(t *testing.T) {
	dir := t.TempDir()
	cfg := completeCfg()
	configPath := filepath.Join(dir, "config.yaml")
	if err := cfg.Save(configPath); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.SetMeta(MetaSetupComplete, "yes")
	blocked := &blockList{Store: storage.NewMemory(), started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	newRunner := func(cfg *config.Config) (*jobs.Runner, error) {
		return &jobs.Runner{Cfg: cfg, State: st, Store: blocked, LockPath: filepath.Join(dir, "job.lock"), Now: time.Now, Log: io.Discard}, nil
	}
	svc, err := New(ctx, configPath, filepath.Join(dir, "job.lock"), st, io.Discard, newRunner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); svc.Close() })
	svc.WriteSettings = func(context.Context, *config.Config) (setup.Archiving, error) { return setup.Ready, nil }
	svc.SetMonitor = func(bool) error { return nil }

	done := make(chan *jobs.Health, 2)
	go func() { done <- svc.Health(context.Background()) }()
	<-blocked.started
	go func() { done <- svc.Health(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	if n := blocked.n.Load(); n != 1 {
		t.Fatalf("overlapping health checks should share one List, got %d", n)
	}
	close(blocked.release)
	h1, h2 := <-done, <-done
	if h1 == nil || h2 == nil {
		t.Fatalf("health = %v, %v", h1, h2)
	}
}
