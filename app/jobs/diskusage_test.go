// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/engines/xtrabackup"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

func TestDiskUsagePostgres(t *testing.T) {
	cfg := config.Default()
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		sql := c.Args[len(c.Args)-1]
		if strings.Contains(sql, "pg_ls_waldir") {
			return []byte("16777216\n"), nil
		}
		return []byte(hex.EncodeToString([]byte("shop\tlive")) + "\t5000000\n" + hex.EncodeToString([]byte("postgres")) + "\t7000\n"), nil
	}}
	r := &Runner{Cfg: cfg, Cluster: postgres.Cluster{Cfg: cfg.Postgres, Run: fake}}
	got, err := r.DiskUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got["db.size:shop\tlive"] != 5e6 || got["db.size:postgres"] != 7000 || got["disk.use:pg_wal"] != 16777216 || len(got) != 3 {
		t.Errorf("got %v", got)
	}
}

func TestDiskUsageMySQL(t *testing.T) {
	cfg := config.Default()
	cfg.Engine = config.EngineMySQL
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		sql := c.Args[len(c.Args)-1]
		switch {
		case strings.Contains(sql, "BINARY LOGS"):
			return []byte("binlog.000001\t1000\tNo\nbinlog.000002\t500\tNo\n"), nil
		case strings.Contains(sql, "stats_expiry = 0"):
			return []byte(hex.EncodeToString([]byte("shop")) + "\t4096\n"), nil
		}
		t.Errorf("unexpected %v", c.Args)
		return nil, nil
	}}
	r := &Runner{Cfg: cfg, XtraBackup: xtrabackup.New(cfg, fake, storage.NewMemory(), t.TempDir())}
	got, err := r.DiskUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got["db.size:shop"] != 4096 || got["disk.use:binlogs"] != 1500 || len(got) != 2 {
		t.Errorf("got %v", got)
	}
}
