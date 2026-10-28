// SPDX-License-Identifier: Apache-2.0

package xtrabackup

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"
)

// RetentionPlan is what to delete. Applying it is Apply's job.
type RetentionPlan struct {
	Backups    []string // labels of complete backups
	Incomplete []string // labels of failed attempts (folders with no manifest)
	Binlogs    []string // binlog names
}

// Empty reports whether there is nothing to delete.
func (p RetentionPlan) Empty() bool {
	return len(p.Backups)+len(p.Incomplete)+len(p.Binlogs) == 0
}

// PlanRetention applies the rules:
//
//   - Keep every full backup taken within keep, and the newest one taken before that,
//     so that every moment within keep can still be restored. Delete older fulls.
//   - A diff goes with its full: kept if its full is kept, deleted otherwise.
//   - Binlogs from before the binlog the oldest kept backup starts in can't be replayed
//     onto anything: delete them.
//   - A failed attempt (incomplete, a label) is deleted once it is a day old, so a
//     backup still running is never touched.
//
// backups are complete backups, oldest first; binlogs are shipped binlog names.
func PlanRetention(backups []Manifest, incomplete, binlogs []string, now time.Time, keep time.Duration) RetentionPlan {
	var plan RetentionPlan
	cutoff := now.Add(-keep)

	keptFull := map[string]bool{}
	var fulls []Manifest
	for _, m := range backups {
		if m.Type == Full {
			fulls = append(fulls, m)
		}
	}
	for i, m := range fulls {
		newestBeforeCutoff := m.Start.Before(cutoff) && (i == len(fulls)-1 || !fulls[i+1].Start.Before(cutoff))
		if !m.Start.Before(cutoff) || newestBeforeCutoff {
			keptFull[m.Label] = true
		}
	}

	oldestKept := ""
	for _, m := range backups {
		kept := keptFull[m.Label] || (m.Type == Diff && keptFull[m.Base])
		if !kept {
			plan.Backups = append(plan.Backups, m.Label)
			continue
		}
		if oldestKept == "" || m.BinlogFile < oldestKept {
			oldestKept = m.BinlogFile
		}
	}

	// With no backup kept (there are none), binlogs have nothing to anchor to: keep them.
	if oldestKept != "" {
		for _, b := range binlogs {
			if b < oldestKept { // zero-padded numbers compare in order
				plan.Binlogs = append(plan.Binlogs, b)
			}
		}
	}

	for _, label := range incomplete {
		if t, err := labelStart(label); err == nil && now.Sub(t) > 24*time.Hour {
			plan.Incomplete = append(plan.Incomplete, label)
		}
	}
	return plan
}

// labelStart reads the start time from a label: 2026-10-25T013000Z-full.
func labelStart(label string) (time.Time, error) {
	if len(label) < len(labelTime) {
		return time.Time{}, errors.New("not a backup label: " + label)
	}
	return time.Parse(labelTime, label[:len(labelTime)])
}

// Incomplete returns the labels of backup folders that have no manifest: attempts that
// failed or were cut short (or one still running).
func (e *Engine) Incomplete(ctx context.Context) ([]string, error) {
	objs, err := e.Store.List(ctx, Prefix)
	if err != nil {
		return nil, err
	}
	hasManifest := map[string]bool{}
	for _, o := range objs {
		label, _, _ := strings.Cut(strings.TrimPrefix(o.Key, Prefix), "/")
		hasManifest[label] = hasManifest[label] || path.Base(o.Key) == ManifestFile
	}
	var labels []string
	for label, ok := range hasManifest {
		if !ok {
			labels = append(labels, label)
		}
	}
	sort.Strings(labels)
	return labels, nil
}

// Apply deletes what the plan lists. A backup's manifest is deleted first, so a
// deletion cut short leaves a failed attempt (cleaned up next time), never a backup
// that looks complete but isn't.
func (e *Engine) Apply(ctx context.Context, plan RetentionPlan) error {
	var errs []error
	for _, label := range append(append([]string(nil), plan.Backups...), plan.Incomplete...) {
		folder := Prefix + label + "/"
		if err := e.Store.Delete(ctx, folder+ManifestFile); err != nil {
			errs = append(errs, err)
			continue
		}
		objs, err := e.Store.List(ctx, folder)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, o := range objs {
			if err := e.Store.Delete(ctx, o.Key); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for _, b := range plan.Binlogs {
		if err := e.Store.Delete(ctx, BinlogPrefix+b+".gz"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
