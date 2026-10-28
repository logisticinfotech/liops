// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"testing"

	"github.com/logisticinfotech/liops/config"
)

func TestBin(t *testing.T) {
	cfg := config.Default().Postgres
	cfg.Version = 16
	if got := (Cluster{Cfg: cfg}).Bin("pg_dump"); got != "/usr/lib/postgresql/16/bin/pg_dump" {
		t.Errorf("default Bin = %q", got)
	}
	cfg.BinDir = "/opt/homebrew/opt/postgresql@16/bin"
	if got := (Cluster{Cfg: cfg}).Bin("pg_dump"); got != "/opt/homebrew/opt/postgresql@16/bin/pg_dump" {
		t.Errorf("Bin with bin_dir = %q", got)
	}
}

func TestLayouts(t *testing.T) {
	cfg := config.Default().Postgres
	debian := Cluster{Cfg: cfg}
	if debian.ServiceName() != "postgresql@18-main" || debian.RestartCommand() != "sudo systemctl restart postgresql@18-main" ||
		debian.ConfDPath() != "/etc/postgresql/18/main/conf.d/10-pgbackrest.conf" {
		t.Errorf("debian: %s, %s, %s", debian.ServiceName(), debian.RestartCommand(), debian.ConfDPath())
	}
	if c := debian.RestartCmd(); c.Name != "systemctl" {
		t.Errorf("debian restart = %+v", c)
	}

	cfg.Layout, cfg.Cluster, cfg.DataDir = config.LayoutHomebrew, "postgresql@18", HomebrewPrefix+"/var/postgresql@18"
	brew := Cluster{Cfg: cfg}
	if brew.ServiceName() != "postgresql@18" || brew.RestartCommand() != "brew services restart postgresql@18" ||
		brew.ConfDPath() != HomebrewPrefix+"/var/postgresql@18/conf.d/10-pgbackrest.conf" {
		t.Errorf("homebrew: %s, %s, %s", brew.ServiceName(), brew.RestartCommand(), brew.ConfDPath())
	}
	if got := brew.Bin("psql"); got != HomebrewPrefix+"/opt/postgresql@18/bin/psql" {
		t.Errorf("homebrew Bin = %q", got)
	}
	if c := brew.RestartCmd(); c.Name != "brew" || c.User != "" {
		t.Errorf("homebrew restart = %+v", c)
	}
}
