// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"testing"

	"github.com/logisticinfotech/pgtijori/config"
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
