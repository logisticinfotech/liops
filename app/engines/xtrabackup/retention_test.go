// SPDX-License-Identifier: Apache-2.0

package xtrabackup

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// backup is a backup taken daysAgo, starting in binlog number n.
func backup(typ string, daysAgo, n int, base string) Manifest {
	start := now.Add(-days(daysAgo))
	return Manifest{Label: start.Format(labelTime) + "-" + typ, Type: typ, Start: start, Base: base, BinlogFile: fmt.Sprintf("binlog.%06d", n)}
}

func TestPlanRetention(t *testing.T) {
	// Weekly fulls 35, 28, 21, 14 and 7 days ago, a diff after each, and binlogs 1-60.
	var backups []Manifest
	for i, age := range []int{35, 28, 21, 14, 7} {
		full := backup(Full, age, i*10+1, "")
		backups = append(backups, full, backup(Diff, age-1, i*10+5, full.Label))
	}
	var binlogs []Binlog
	for n := 1; n <= 60; n++ {
		binlogs = append(binlogs, Binlog{Name: fmt.Sprintf("binlog.%06d", n)})
	}
	plan := PlanRetention(backups, nil, binlogs, now, days(30))

	// The full from 35 days ago is the newest one before the 30-day cutoff: it stays, so
	// the moment 30 days ago can still be restored. Nothing is older, so nothing goes.
	if len(plan.Backups) != 0 {
		t.Errorf("deleted backups %q, want none", plan.Backups)
	}
	if len(plan.Binlogs) != 0 {
		t.Errorf("deleted binlogs %q, want none (the oldest kept backup starts in binlog 1)", plan.Binlogs)
	}

	// A week later the 35-day-old full (now 42) and its diff go, and binlogs before the
	// next full's (number 11).
	later := now.Add(days(7))
	plan = PlanRetention(backups, nil, binlogs, later, days(30))
	if want := []string{backups[0].Label, backups[1].Label}; !slices.Equal(plan.Backups, want) {
		t.Errorf("deleted backups %q, want %q", plan.Backups, want)
	}
	if want := binlogs[:10]; !slices.Equal(plan.Binlogs, want) {
		t.Errorf("deleted binlogs %+v, want %+v", plan.Binlogs, want)
	}
}

func TestPlanRetentionKeepsTheNewestFull(t *testing.T) {
	// Only old backups: nothing newer replaces them, so the newest full and its diff stay.
	old := backup(Full, 90, 1, "")
	older := backup(Full, 120, 1, "")
	diff := backup(Diff, 89, 2, old.Label)
	plan := PlanRetention([]Manifest{older, old, diff}, nil, []Binlog{{Name: "binlog.000001"}}, now, days(30))
	if !slices.Equal(plan.Backups, []string{older.Label}) || len(plan.Binlogs) != 0 {
		t.Errorf("plan = %+v", plan)
	}
}

func TestPlanRetentionDropsAnOldGeneration(t *testing.T) {
	old := backup(Full, 90, 50, "")
	old.BinlogGen = "gen1"
	anchor := backup(Full, 40, 1, "")
	anchor.BinlogGen = "gen2"
	neu := backup(Full, 1, 2, "")
	neu.BinlogGen = "gen2"
	binlogs := []Binlog{
		{Name: "binlog.000050", Gen: "gen1"},
		{Name: "binlog.000051", Gen: "gen1"},
		{Name: "binlog.000001", Gen: "gen2"},
	}
	plan := PlanRetention([]Manifest{old, anchor, neu}, nil, binlogs, now, days(30))
	if !slices.Equal(plan.Backups, []string{old.Label}) {
		t.Errorf("deleted backups %q, want the expired generation's full", plan.Backups)
	}
	if !slices.Equal(plan.Binlogs, []Binlog{{Name: "binlog.000050", Gen: "gen1"}, {Name: "binlog.000051", Gen: "gen1"}}) {
		t.Errorf("deleted binlogs %+v, want the old generation", plan.Binlogs)
	}
}

func TestPlanRetentionCleansUpFailedAttempts(t *testing.T) {
	recent := now.Add(-time.Hour).Format(labelTime) + "-full" // perhaps still running
	stale := now.Add(-days(2)).Format(labelTime) + "-diff"
	plan := PlanRetention(nil, []string{recent, stale}, []Binlog{{Name: "binlog.000001"}}, now, days(30))
	if !slices.Equal(plan.Incomplete, []string{stale}) {
		t.Errorf("incomplete to delete = %q, want only the one older than a day", plan.Incomplete)
	}
	if len(plan.Binlogs) != 0 {
		t.Error("with no backups, binlogs have nothing to anchor to and are kept")
	}
}

func TestApplyRetention(t *testing.T) {
	f := &fakeXtraBackup{}
	e, store := newEngine(t, f)
	ctx := context.Background()
	full, _ := e.Backup(ctx, Full)
	diff, _ := e.Backup(ctx, Diff)
	store.Add(BinlogPrefix+"binlog.000001.gz", []byte("x"), now)
	store.Add(Prefix+"2026-10-01T000000Z-full/"+StreamFile, []byte("cut short"), now)

	incomplete, err := e.Incomplete(ctx)
	if err != nil || !slices.Equal(incomplete, []string{"2026-10-01T000000Z-full"}) {
		t.Fatalf("Incomplete = %q, %v", incomplete, err)
	}
	if err := e.Apply(ctx, RetentionPlan{Backups: []string{full.Label, diff.Label}, Incomplete: incomplete, Binlogs: []Binlog{{Name: "binlog.000001"}}}); err != nil {
		t.Fatal(err)
	}
	if objs, _ := store.List(ctx, ""); len(objs) != 0 {
		t.Errorf("left behind: %v", objs)
	}
}
