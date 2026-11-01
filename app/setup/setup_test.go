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
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/storage"
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
	missing := `[{"name":"app-prod-1","db":[],"backup":[],"archive":[],"status":{"code":1,"message":"missing stanza path"}}]`
	ours := `[{"name":"app-prod-1","db":[{"id":1,"system-id":` + mine + `}],"backup":[],"archive":[],"status":{"code":0,"message":"ok"}}]`
	theirs := `[{"name":"app-prod-1","db":[{"id":1,"system-id":7000000000000000001}],"backup":[],"archive":[],"status":{"code":0,"message":"ok"}}]`
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
	const myMarker = OwnersPrefix + "postgres-" + mine

	for _, c := range []struct {
		name    string
		objects []string // already in the folder
		info    string
		want    string // "" when it's this cluster's to use; else part of the refusal
	}{
		{"a new name", nil, missing, ""},
		{"setup run again", []string{myMarker, "pgbackrest/backup/app-prod-1/backup.info"}, ours, ""},
		{"from before markers, this cluster's stanza", []string{"pgbackrest/backup/app-prod-1/backup.info"}, ours, ""},
		{"from before markers, another cluster's stanza", []string{"pgbackrest/backup/app-prod-1/backup.info"}, theirs, "system identifier 7000000000000000001"},
		{"another PostgreSQL server's marker", []string{OwnersPrefix + "postgres-7000000000000000001"}, missing, "another PostgreSQL server"},
		{"a MySQL server's marker", []string{OwnersPrefix + "mysql-3e11fa47-71ca-11e1-9e33-c80aa9429562", "dumps/app/x.sql.gz"}, missing, "another MySQL server"},
		{"dumps no server has claimed", []string{"dumps/app/2026-10-01T013000Z.dump"}, missing, "no server has claimed"},
		{"a leftover storage probe", []string{"liops-probe/abc"}, missing, ""},
	} {
		store := storage.NewMemory()
		for _, key := range c.objects {
			store.Add(key, []byte("x"), time.Now())
		}
		err := CheckOwner(ctx, engine(c.info), store)
		_, marked := store.Get(myMarker)
		if c.want == "" {
			if err != nil || !marked {
				t.Errorf("%s: want it claimed, got %v (marker written: %v)", c.name, err, marked)
			}
			continue
		}
		var taken *NameTakenError
		if !errors.As(err, &taken) || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "restore.md") ||
			!strings.Contains(err.Error(), "Choose a new backup name (pgbackrest.stanza)") {
			t.Errorf("%s: want a refusal mentioning %q, got: %v", c.name, c.want, err)
		}
		if marked {
			t.Errorf("%s: a refused folder must not get this cluster's marker", c.name)
		}
	}
}

type countList struct {
	storage.Store
	lists int
}

func (c *countList) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	if prefix == "" {
		c.lists++
	}
	return c.Store.List(ctx, prefix)
}

func TestFolderInUseStopsBeforeListingWAL(t *testing.T) {
	mem := storage.NewMemory()
	for i := 0; i < 100; i++ {
		mem.Add(fmt.Sprintf("pgbackrest/archive/18-1/%d", i), []byte("wal"), time.Now())
	}
	c := &countList{Store: mem}
	used, err := folderInUse(context.Background(), c)
	if err != nil || !used {
		t.Fatalf("used=%v err=%v", used, err)
	}
	if c.lists != 0 {
		t.Errorf("a pgBackRest repo must be seen with Any, not a full List; lists=%d", c.lists)
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

	store := storage.NewMemory()
	store.Add(OwnersPrefix+"postgres-7000000000000000001", []byte("x"), time.Now())
	var taken *NameTakenError
	if _, err := WriteSettings(context.Background(), e, store); !errors.As(err, &taken) {
		t.Fatalf("expected NameTakenError, got %v", err)
	}
	if _, err := os.Stat(e.Cluster.ConfDPath()); !errors.Is(err, os.ErrNotExist) {
		t.Error("WAL archiving settings must not be written when the backup name is taken")
	}
	if conf, _ := os.ReadFile(filepath.Join(cfg.Postgres.DataDir, "postgresql.conf")); strings.Contains(string(conf), "include_dir") {
		t.Error("postgresql.conf must be left alone when the backup name is taken")
	}
}
