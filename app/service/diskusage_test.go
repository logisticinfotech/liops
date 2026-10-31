// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/jobs"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/config"
)

func TestPushDiskUsage(t *testing.T) {
	oldPoll, oldEvery := diskUsagePoll, diskUsageEvery
	diskUsagePoll, diskUsageEvery = 5*time.Millisecond, time.Hour
	t.Cleanup(func() { diskUsagePoll, diskUsageEvery = oldPoll, oldEvery })

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
	st.SetMeta(MetaSetupComplete, "yes")
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		if strings.Contains(c.Args[len(c.Args)-1], "pg_ls_waldir") {
			return []byte("100\n"), nil
		}
		return []byte("617070\t42\n"), nil // "app"
	}}
	newRunner := func(cfg *config.Config) (*jobs.Runner, error) {
		return &jobs.Runner{Cfg: cfg, State: st, LockPath: filepath.Join(dir, "job.lock"), Now: time.Now, Log: io.Discard,
			Cluster: postgres.Cluster{Cfg: cfg.Postgres, Run: fake}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc, err := New(ctx, configPath, filepath.Join(dir, "job.lock"), st, io.Discard, newRunner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); svc.Close() })

	var mu sync.Mutex
	var pushes []map[string]float64
	fail := true
	done := make(chan struct{})
	go func() {
		svc.PushDiskUsage(func(_ context.Context, v map[string]float64) error {
			mu.Lock()
			defer mu.Unlock()
			pushes = append(pushes, v)
			if fail { // the monitor isn't up yet: try again at the next poll
				fail = false
				return errors.New("the monitor isn't running")
			}
			return nil
		})
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(pushes) != 0 {
		t.Errorf("monitoring is off, but sizes were pushed: %v", pushes)
	}
	mu.Unlock()

	cfg.Monitor.Enabled = true
	svc.SetMonitor = func(bool) error { return nil }
	svc.WriteSettings = func(context.Context, *config.Config) (setup.Archiving, error) { return setup.Ready, nil }
	if err := svc.SaveSettings(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if len(pushes) != 2 || pushes[1]["db.size:app"] != 42 || pushes[1]["disk.use:pg_wal"] != 100 {
		t.Errorf("want a failed push, then one success and no more within the hour: %v", pushes)
	}
	mu.Unlock()
	cancel()
	<-done
}
