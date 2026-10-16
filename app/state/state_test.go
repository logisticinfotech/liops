// SPDX-License-Identifier: Apache-2.0

package state

import (
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "sub", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRunsLifecycle(t *testing.T) {
	s := open(t)
	base := time.Date(2026, 10, 1, 1, 30, 0, 0, time.UTC)
	for i, job := range []string{"full", "dumps", "diff", "dumps"} {
		r := &Run{Job: job, Trigger: "schedule", Start: base.Add(time.Duration(i) * 24 * time.Hour)}
		if err := s.StartRun(r); err != nil {
			t.Fatal(err)
		}
		if r.ID != uint64(i+1) || r.Status != StatusRunning {
			t.Fatalf("run %d: %+v", i, r)
		}
		r.Status, r.End = StatusOK, r.Start.Add(time.Minute)
		if i == 3 {
			r.Status = StatusFailed
			r.Dumps = []DumpRun{{DB: "app", Error: "boom"}}
		}
		if err := s.SaveRun(r); err != nil {
			t.Fatal(err)
		}
	}

	runs, err := s.Runs("", 10)
	if err != nil || len(runs) != 4 || runs[0].ID != 4 {
		t.Fatalf("Runs should be newest first: %v %+v", err, runs)
	}
	if runs, _ := s.Runs("dumps", 1); len(runs) != 1 || runs[0].ID != 4 || runs[0].Dumps[0].Error != "boom" {
		t.Errorf("Runs(dumps, 1) = %+v", runs)
	}
	if r, _ := s.LastRun("dumps", StatusOK); r == nil || r.ID != 2 {
		t.Errorf("LastRun(dumps, ok) = %+v", r)
	}
	if r, _ := s.LastRun("report", ""); r != nil {
		t.Errorf("LastRun of a job that never ran = %+v", r)
	}

	n, err := s.PruneRuns(base.Add(2 * 24 * time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("PruneRuns removed %d, %v", n, err)
	}
	if runs, _ := s.Runs("", 10); len(runs) != 2 || runs[1].ID != 3 {
		t.Errorf("after prune: %+v", runs)
	}
}

func TestMarkInterrupted(t *testing.T) {
	s := open(t)
	r := &Run{Job: "diff", Start: time.Now()}
	if err := s.StartRun(r); err != nil {
		t.Fatal(err)
	}
	if n, err := s.MarkInterrupted(time.Now()); err != nil || n != 1 {
		t.Fatalf("MarkInterrupted = %d, %v", n, err)
	}
	if got, _ := s.LastRun("diff", ""); got.Status != StatusInterrupted {
		t.Errorf("status = %q", got.Status)
	}
}

func TestDeleted(t *testing.T) {
	s := open(t)
	since := time.Date(2026, 10, 4, 2, 0, 0, 0, time.UTC)
	if err := s.SetDeleted("tenant_xyz", since); err != nil {
		t.Fatal(err)
	}
	m, err := s.Deleted()
	if err != nil || !m["tenant_xyz"].Equal(since) {
		t.Fatalf("Deleted = %v, %v", m, err)
	}
	if err := s.ClearDeleted("tenant_xyz"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.Deleted(); len(m) != 0 {
		t.Errorf("after clear: %v", m)
	}
}

func TestAuditPaging(t *testing.T) {
	s := open(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		if err := s.Audit(AuditEntry{Time: base.Add(time.Duration(i) * time.Hour), User: "ui", Action: "download", OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	page, _ := s.AuditLog(0, 2)
	if len(page) != 2 || page[0].ID != 5 || page[1].ID != 4 {
		t.Fatalf("first page: %+v", page)
	}
	page, _ = s.AuditLog(page[1].ID, 10)
	if len(page) != 3 || page[0].ID != 3 {
		t.Fatalf("second page: %+v", page)
	}
	if n, _ := s.PruneAudit(base.Add(2 * time.Hour)); n != 2 {
		t.Errorf("pruned %d", n)
	}
	if err := s.SetMeta("setup_complete", "yes"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetMeta("setup_complete"); v != "yes" {
		t.Errorf("meta = %q", v)
	}
}
