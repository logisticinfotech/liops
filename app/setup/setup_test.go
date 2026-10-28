// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/config"
)

func TestParseClusters(t *testing.T) {
	out := `16  main    5433 down   postgres /var/lib/postgresql/16/main /var/log/postgresql/postgresql-16-main.log
18  main    5432 online postgres /var/lib/postgresql/18/main /var/log/postgresql/postgresql-18-main.log
garbage line
`
	clusters := ParseClusters(out)
	if len(clusters) != 2 {
		t.Fatalf("clusters = %+v", clusters)
	}
	c := clusters[1]
	if c.Version != 18 || c.Name != "main" || c.Port != 5432 || !c.Online() || c.DataDir != "/var/lib/postgresql/18/main" {
		t.Errorf("cluster = %+v", c)
	}
	if clusters[0].Online() {
		t.Error("a down cluster isn't online")
	}

	cfg := config.Default()
	Apply(cfg, clusters[0])
	if cfg.Postgres.Version != 16 || cfg.Postgres.Port != 5433 || ServiceName(cfg) != "postgresql@16-main" {
		t.Errorf("Apply: %+v", cfg.Postgres)
	}
}

func TestDetectHomebrew(t *testing.T) {
	prefix := t.TempDir()
	mk := func(name, version, conf, pid string) {
		dir := filepath.Join(prefix, "var", name)
		os.MkdirAll(dir, 0o700)
		if version != "" {
			os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte(version+"\n"), 0o600)
		}
		os.WriteFile(filepath.Join(dir, "postgresql.conf"), []byte(conf), 0o600)
		if pid != "" {
			os.WriteFile(filepath.Join(dir, "postmaster.pid"), []byte(pid), 0o600)
		}
	}
	running := fmt.Sprintf("%d\n%s/var/postgresql@18\n1791163801\n5433\n/tmp\n", os.Getpid(), prefix)
	mk("postgresql@18", "18", "#port = 5432\n", running)
	mk("postgresql@17", "17", "port = 5440\n", "")
	mk("postgresql@16", "", "", "") // no PG_VERSION: not initialised

	clusters, err := DetectHomebrew(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("clusters = %+v", clusters)
	}
	byVersion := map[int]Cluster{clusters[0].Version: clusters[0], clusters[1].Version: clusters[1]}
	c18, c17 := byVersion[18], byVersion[17]
	if !c18.Online() || c18.Port != 5433 || c18.Name != "postgresql@18" || c18.Layout != config.LayoutHomebrew ||
		c18.BinDir != filepath.Join(prefix, "opt", "postgresql@18", "bin") || c18.Owner == "" {
		t.Errorf("18 = %+v", c18)
	}
	if c17.Online() || c17.Port != 5440 {
		t.Errorf("17 = %+v", c17)
	}
	if c17.StartCommand() != "brew services start postgresql@17" {
		t.Errorf("start = %q", c17.StartCommand())
	}

	cfg := config.Default()
	Apply(cfg, c18)
	if cfg.Postgres.Layout != config.LayoutHomebrew || cfg.Postgres.BinDir != c18.BinDir || RestartCommand(cfg) != "brew services restart postgresql@18" {
		t.Errorf("Apply: %+v", cfg.Postgres)
	}
}

func TestDetectClustersFallsBackToHomebrewOnMac(t *testing.T) {
	missing := &command.Fake{Handle: func(string, command.Cmd) ([]byte, error) {
		return nil, errors.New("executable file not found")
	}}
	oldGOOS, oldPrefix := goos, postgres.HomebrewPrefix
	t.Cleanup(func() { goos, postgres.HomebrewPrefix = oldGOOS, oldPrefix })
	postgres.HomebrewPrefix = t.TempDir()

	goos = "linux"
	if _, err := DetectClusters(context.Background(), missing); err == nil || !strings.Contains(err.Error(), "PGDG") {
		t.Errorf("linux without pg_lsclusters: %v", err)
	}
	goos = "darwin"
	if clusters, err := DetectClusters(context.Background(), missing); err != nil || len(clusters) != 0 {
		t.Errorf("darwin with no Homebrew clusters: %+v, %v", clusters, err)
	}
}

func TestCheckOwner(t *testing.T) {
	const mine = "7683116411252273504"
	engine := func(infoJSON string) *pgbackrest.Engine {
		cfg := config.Default()
		cfg.PgBackRest.Stanza = "app-prod-1"
		return pgbackrest.New(cfg, &command.Fake{Handle: func(program string, c command.Cmd) ([]byte, error) {
			if program == "psql" {
				return []byte(mine + "\n"), nil
			}
			return []byte(infoJSON), nil
		}})
	}
	ctx := context.Background()

	missing := `[{"name":"app-prod-1","db":[],"backup":[],"archive":[],"status":{"code":1,"message":"missing stanza path"}}]`
	if err := CheckOwner(ctx, engine(missing)); err != nil {
		t.Errorf("a new backup name should be fine: %v", err)
	}
	ours := `[{"name":"app-prod-1","db":[{"id":1,"system-id":` + mine + `}],"backup":[],"archive":[],"status":{"code":0,"message":"ok"}}]`
	if err := CheckOwner(ctx, engine(ours)); err != nil {
		t.Errorf("this cluster's own backups should be fine: %v", err)
	}
	theirs := `[{"name":"app-prod-1","db":[{"id":1,"system-id":7000000000000000001}],"backup":[],"archive":[],"status":{"code":0,"message":"ok"}}]`
	err := CheckOwner(ctx, engine(theirs))
	var taken *NameTakenError
	if !errors.As(err, &taken) || !strings.Contains(err.Error(), "restore.md") || !strings.Contains(err.Error(), "Choose a new backup name") {
		t.Errorf("another cluster's backups must be refused with a clear message, got: %v", err)
	}
}

func TestWriteSettingsStopsBeforeArchivingWhenNameIsTaken(t *testing.T) {
	old := postgres.HomebrewPrefix
	postgres.HomebrewPrefix = t.TempDir()
	t.Cleanup(func() { postgres.HomebrewPrefix = old })
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.PgBackRest.Stanza = "app-prod-1"
	cfg.Postgres.Layout = config.LayoutHomebrew
	cfg.Postgres.DataDir = t.TempDir()
	cfg.Postgres.OSUser = me.Username
	os.WriteFile(filepath.Join(cfg.Postgres.DataDir, "postgresql.conf"), []byte("port = 5432\n"), 0o600)
	theirs := `[{"name":"app-prod-1","db":[{"id":1,"system-id":7000000000000000001}],"backup":[],"archive":[],"status":{"code":0,"message":"ok"}}]`
	e := pgbackrest.New(cfg, &command.Fake{Handle: func(program string, c command.Cmd) ([]byte, error) {
		switch program {
		case "psql":
			return []byte("7683116411252273504\n"), nil
		case "pgbackrest":
			return []byte(theirs), nil
		}
		return nil, nil // test -r
	}})

	var taken *NameTakenError
	if _, err := WriteSettings(context.Background(), e); !errors.As(err, &taken) {
		t.Fatalf("expected NameTakenError, got %v", err)
	}
	if _, err := os.Stat(e.Cluster.ConfDPath()); !errors.Is(err, os.ErrNotExist) {
		t.Error("WAL archiving settings must not be written when the backup name is taken")
	}
	if conf, _ := os.ReadFile(filepath.Join(cfg.Postgres.DataDir, "postgresql.conf")); strings.Contains(string(conf), "include_dir") {
		t.Error("postgresql.conf must be left alone when the backup name is taken")
	}
}
