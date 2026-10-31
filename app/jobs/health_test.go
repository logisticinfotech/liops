// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/config"
)

func TestServerHealthInTheDailySummary(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "lj") // a short socket path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/db", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]monitor.DBStatus{{Label: "mysql", Title: "MySQL", Engine: "mysql", Statements: true}})
	})
	mux.HandleFunc("GET /v1/db/queries", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]monitor.QueryTotal{
			{QueryShape: monitor.QueryShape{DB: "tenant_a", Text: "SELECT * FROM `customers` WHERE LOWER ( `name` ) LIKE ?"},
				QueryCounters: monitor.QueryCounters{Calls: 900, TimeMS: 41 * 60e3, RowsExamined: 900 * 1.65e6, RowsSent: 900}},
			{QueryShape: monitor.QueryShape{DB: "tenant_a", Text: "SELECT * FROM `users` WHERE `id` = ?"},
				QueryCounters: monitor.QueryCounters{Calls: 1e5, TimeMS: 5000, RowsExamined: 1e5, RowsSent: 1e5}},
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socket := filepath.Join(dir, "m.sock")
	go monitor.Serve(ctx, socket, mux)
	for range 100 {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cfg := config.Default()
	r := &Runner{Cfg: cfg, Monitor: monitor.NewClient(socket)}
	if lines := r.serverHealth(ctx); lines != nil {
		t.Errorf("monitoring off: %q", lines)
	}
	cfg.Monitor.Enabled = true
	lines := r.serverHealth(ctx)
	_, body := notify.DailySummary(notify.Summary{Server: "db-test", Now: time.Now(), Health: lines})
	for _, want := range []string{"Server health:", "MySQL: 1 query scans whole tables: 41 min of database time", "1.65M rows read per row returned",
		"LOWER ( `name` ) LIKE ?", "Monitoring → Database"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary should contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "users") {
		t.Errorf("a lookup by key isn't a scan:\n%s", body)
	}

	r.Monitor = monitor.NewClient(filepath.Join(dir, "none.sock"))
	if lines := r.serverHealth(ctx); len(lines) != 1 || !strings.Contains(lines[0], "isn't answering") {
		t.Errorf("monitor down: %q", lines)
	}
}
