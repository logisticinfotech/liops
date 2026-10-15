// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"strings"
	"testing"

	"github.com/logisticinfotech/pgtijori/config"
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

func TestToken(t *testing.T) {
	path := t.TempDir() + "/sub/setup-token"
	if CheckToken(path, "") {
		t.Fatal("no token file accepts nothing")
	}
	tok, err := NewToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 27 || !CheckToken(path, tok) || !CheckToken(path, " "+strings.ToLower(strings.ReplaceAll(tok, "-", ""))+" ") {
		t.Errorf("token %q should be accepted however it's typed", tok)
	}
	if CheckToken(path, "AAAAAA-AAAAAA-AAAAAA-AAAAAA") || CheckToken(path, "") {
		t.Error("a wrong token must be rejected")
	}
	if err := RemoveToken(path); err != nil || CheckToken(path, tok) {
		t.Error("a removed token must be rejected")
	}
}
