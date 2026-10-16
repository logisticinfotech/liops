// SPDX-License-Identifier: Apache-2.0

package pgdump

import (
	"slices"
	"testing"
	"time"

	"github.com/logisticinfotech/pgtijori/app/storage"
)

var now = time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

const retentionStanza = "main"

// stored builds dumps of db taken the given number of days before now.
func stored(db string, ages ...int) []Dump {
	var d []Dump
	for _, a := range ages {
		t := now.Add(-days(a))
		d = append(d, Dump{Key: Key(retentionStanza, db, t), Taken: t})
	}
	return d
}

func TestRetentionLiveDatabase(t *testing.T) {
	plan := PlanRetention(RetentionInput{
		Stored: map[string][]Dump{"app": stored("app", 10, 8, 7, 1, 0)},
		Live:   []string{"app"},
		Now:    now, Keep: days(7), KeepDeleted: days(14),
	})
	want := []string{Key(retentionStanza, "app", now.Add(-days(10))), Key(retentionStanza, "app", now.Add(-days(8)))}
	if !slices.Equal(plan.Delete, want) {
		t.Errorf("Delete = %v, want %v", plan.Delete, want)
	}
}

func TestRetentionKeepsNewestEvenIfOld(t *testing.T) {
	plan := PlanRetention(RetentionInput{
		Stored: map[string][]Dump{"app": stored("app", 30, 20)},
		Live:   []string{"app"},
		Now:    now, Keep: days(7), KeepDeleted: days(14),
	})
	if !slices.Equal(plan.Delete, []string{Key(retentionStanza, "app", now.Add(-days(30)))}) {
		t.Errorf("the newest dump must survive a run of failed nights: %v", plan.Delete)
	}
}

func TestRetentionNewlyDeletedDatabase(t *testing.T) {
	plan := PlanRetention(RetentionInput{
		Stored: map[string][]Dump{"gone": stored("gone", 3, 2, 1)},
		Live:   []string{"app"},
		Now:    now, Keep: days(7), KeepDeleted: days(14),
	})
	if !slices.Equal(plan.NewlyDeleted, []string{"gone"}) {
		t.Errorf("NewlyDeleted = %v", plan.NewlyDeleted)
	}
	if len(plan.Delete) != 2 || slices.Contains(plan.Delete, Key(retentionStanza, "gone", now.Add(-days(1)))) {
		t.Errorf("only the newest dump of a gone database is kept: %v", plan.Delete)
	}
}

func TestRetentionDeletedDatabaseExpires(t *testing.T) {
	in := RetentionInput{
		Stored:       map[string][]Dump{"gone": stored("gone", 20)},
		DeletedSince: map[string]time.Time{"gone": now.Add(-days(10))},
		Now:          now, Keep: days(7), KeepDeleted: days(14),
	}
	if plan := PlanRetention(in); len(plan.Delete) != 0 || len(plan.Forgotten) != 0 || len(plan.NewlyDeleted) != 0 {
		t.Errorf("within KeepDeleted nothing changes: %+v", plan)
	}
	in.DeletedSince["gone"] = now.Add(-days(15))
	plan := PlanRetention(in)
	if len(plan.Delete) != 1 || !slices.Equal(plan.Forgotten, []string{"gone"}) {
		t.Errorf("after KeepDeleted the last dump goes: %+v", plan)
	}
}

func TestRetentionReappearedAndOrphans(t *testing.T) {
	plan := PlanRetention(RetentionInput{
		Stored: map[string][]Dump{"back": stored("back", 1)},
		Live:   []string{"back"},
		DeletedSince: map[string]time.Time{
			"back":     now.Add(-days(2)),
			"vanished": now.Add(-days(2)), // tracked, but its dumps are already gone
		},
		Now: now, Keep: days(7), KeepDeleted: days(14),
	})
	if !slices.Equal(plan.Reappeared, []string{"back"}) || !slices.Equal(plan.Forgotten, []string{"vanished"}) {
		t.Errorf("plan = %+v", plan)
	}
}

func TestGroupIgnoresForeignKeys(t *testing.T) {
	objs := []storage.Object{
		{Key: Key(retentionStanza, "app", now)},
		{Key: Key(retentionStanza, "app", now.Add(-days(1)))},
		{Key: "dumps/readme.txt"},
	}
	g := Group(objs)
	if len(g) != 1 || len(g["app"]) != 2 || !g["app"][0].Taken.Before(g["app"][1].Taken) {
		t.Errorf("Group = %+v", g)
	}
}

func TestGlobalsRetention(t *testing.T) {
	var objs []storage.Object
	for _, a := range []int{9, 8, 1} {
		at := now.Add(-days(a))
		objs = append(objs, storage.Object{Key: GlobalsKey(retentionStanza, at), Modified: at})
	}
	if del := PlanGlobalsRetention(objs, now, days(7)); len(del) != 2 {
		t.Errorf("delete = %v", del)
	}
	if del := PlanGlobalsRetention(objs[:2], now, days(7)); len(del) != 1 {
		t.Errorf("the newest globals dump is always kept: %v", del)
	}
}
