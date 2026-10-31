// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

// fill adds a sample every 15 s for d: "load" is 10 in the first 15 s of each minute and 2
// otherwise (average 4, maximum 10); "disk" is the minute number since t0.
func fill(t *testing.T, s *Store, start time.Time, d time.Duration) {
	t.Helper()
	for at := start; at.Before(start.Add(d)); at = at.Add(15 * time.Second) {
		load := 2.0
		if at.Second() == 0 {
			load = 10
		}
		if err := s.Add(Sample{Time: at, Values: map[string]float64{"load": load, "disk": float64(at.Sub(t0) / time.Minute)}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoreRollsUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	fill(t, s, t0, 3*time.Hour)
	end := t0.Add(3 * time.Hour)

	// The last hour: every sample.
	raw, _ := s.Query([]string{"load"}, end.Add(-time.Hour), end)
	if raw.Step != 15*time.Second || len(raw.X) != 240 {
		t.Errorf("last hour: step %v, %d points", raw.Step, len(raw.X))
	}

	// A day: minutes, average 4 and maximum 10 each (the minute in progress isn't written yet).
	day, _ := s.Query([]string{"load", "disk"}, end.Add(-24*time.Hour), end)
	if day.Step != time.Minute || len(day.X) != 179 {
		t.Fatalf("a day: step %v, %d points", day.Step, len(day.X))
	}
	if v := *day.Values["load"][0]; v != 4 {
		t.Errorf("minute average = %v, want 4", v)
	}
	if v := *day.Max["load"][0]; v != 10 {
		t.Errorf("minute maximum = %v, want 10 (spikes must survive the rollup)", v)
	}
	if day.X[5] != t0.Add(5*time.Minute).Unix() || *day.Values["disk"][5] != 5 {
		t.Errorf("minute 5: x %d, disk %v", day.X[5], *day.Values["disk"][5])
	}

	// 90 days: hours, written once each hour is over.
	quarter, _ := s.Query([]string{"load"}, end.Add(-90*24*time.Hour), end)
	if quarter.Step != time.Hour || len(quarter.X) != 2 || *quarter.Values["load"][0] != 4 || *quarter.Max["load"][0] != 10 {
		t.Errorf("hours: step %v, %d points, %v", quarter.Step, len(quarter.X), quarter.Values["load"])
	}

	// The history outlives a restart.
	s.Close()
	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	again, _ := s.Query([]string{"load", "missing"}, end.Add(-24*time.Hour), end)
	if len(again.X) != 179 || *again.Values["load"][0] != 4 || again.Values["missing"][0] != nil {
		t.Errorf("after reopening: %d points", len(again.X))
	}
	if names := s.Names(); len(names) != 2 {
		t.Errorf("Names = %q", names)
	}
}

func TestStorePrunes(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fill(t, s, t0, 2*time.Hour)
	if err := s.Prune(t0.Add(8 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(8 * 24 * time.Hour)
	if got, _ := s.Query([]string{"load"}, later.Add(-7*24*time.Hour-time.Minute), later); len(got.X) != 0 {
		t.Errorf("minutes older than 7 days should be gone, %d left", len(got.X))
	}
	if got, _ := s.Query([]string{"load"}, later.Add(-90*24*time.Hour), later); len(got.X) != 1 {
		t.Errorf("hours are kept 90 days: %d", len(got.X))
	}
}
