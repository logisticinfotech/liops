// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"path/filepath"
	"testing"
	"time"
)

func TestQueryRoutes(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	// Eight 5-minute periods: /orders traffic varies, and the lookup runs 40 times per
	// /orders request; /health is steady and unrelated.
	orders := []float64{10, 30, 5, 50, 20, 0, 40, 15}
	for i, n := range orders {
		at := start.Add(time.Duration(i) * 5 * time.Minute)
		store.AddQueries(at, []QueryTotal{{QueryShape: QueryShape{Server: "mysql", DB: "shop", ID: "abc", Text: "SELECT * FROM `products` WHERE `id` = ?"},
			QueryCounters: QueryCounters{Calls: 40*n + 1, TimeMS: 1}}})
		store.writeRoutes(bucketReqMinute, uint32(at.Unix()/60), map[routeKey]*RouteStats{
			{"shop.example.com", "GET /orders/{n}"}: {Count: n},
			{"shop.example.com", "GET /health"}:     {Count: 12},
		})
	}
	got, err := store.QueryRoutes("mysql", "shop", "abc", start.Add(-time.Minute), start.Add(time.Hour))
	if err != nil || len(got) != 1 || got[0].Route != "GET /orders/{n}" || got[0].Correlation < 0.99 || got[0].PerRequest < 39 || got[0].PerRequest > 41 {
		t.Errorf("got %+v, %v", got, err)
	}
}
