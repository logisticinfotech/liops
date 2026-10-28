// SPDX-License-Identifier: Apache-2.0

package pgdump

import (
	"sort"
	"time"

	"github.com/logisticinfotech/liops/app/storage"
)

// Dump is one stored dump of a database.
type Dump struct {
	Key   string
	Taken time.Time
	Size  int64
}

// Group sorts dump objects by database, each list oldest first. Objects whose keys
// don't look like dumps are ignored.
func Group(objs []storage.Object) map[string][]Dump {
	byDB := map[string][]Dump{}
	for _, o := range objs {
		db, t, ok := ParseKey(o.Key)
		if !ok {
			continue
		}
		byDB[db] = append(byDB[db], Dump{Key: o.Key, Taken: t, Size: o.Size})
	}
	for _, dumps := range byDB {
		sort.Slice(dumps, func(i, j int) bool { return dumps[i].Taken.Before(dumps[j].Taken) })
	}
	return byDB
}

// RetentionInput is everything the retention rules look at.
type RetentionInput struct {
	Stored       map[string][]Dump    // from Group
	Live         []string             // databases that are dumped (discovered minus excluded)
	DeletedSince map[string]time.Time // databases already known to be gone, and since when
	Now          time.Time
	Keep         time.Duration // how long dumps of live databases are kept
	KeepDeleted  time.Duration // how long the last dump of a gone database is kept
}

// RetentionPlan is what to change. Applying it is the caller's job.
type RetentionPlan struct {
	Delete       []string // object keys
	NewlyDeleted []string // databases that disappeared: record them as deleted since Now
	Reappeared   []string // databases that are back: forget they were deleted
	Forgotten    []string // gone databases whose last dump is now removed: stop tracking them
}

// PlanRetention applies the rules:
//
//   - Live database: delete dumps older than Keep, but always keep the newest one, so
//     a run of failed nights can never leave a database with no dump at all.
//   - Gone database (dropped, or newly excluded): keep only its newest dump, for
//     KeepDeleted after it was first noticed missing, then delete that too.
//
// Callers must only plan retention after a successful discovery: an empty Live list
// from a failed query would mark every database as gone.
func PlanRetention(in RetentionInput) RetentionPlan {
	var plan RetentionPlan
	live := map[string]bool{}
	for _, db := range in.Live {
		live[db] = true
	}

	for _, db := range sortedKeys(in.Stored) {
		dumps := in.Stored[db]
		newest := len(dumps) - 1
		if live[db] {
			for i, d := range dumps {
				if i != newest && in.Now.Sub(d.Taken) > in.Keep {
					plan.Delete = append(plan.Delete, d.Key)
				}
			}
			continue
		}

		since, known := in.DeletedSince[db]
		if !known {
			since = in.Now
			plan.NewlyDeleted = append(plan.NewlyDeleted, db)
		}
		expired := in.Now.Sub(since) > in.KeepDeleted
		for i, d := range dumps {
			if i != newest || expired {
				plan.Delete = append(plan.Delete, d.Key)
			}
		}
		if expired {
			plan.Forgotten = append(plan.Forgotten, db)
		}
	}

	for _, db := range sortedKeys(in.DeletedSince) {
		switch {
		case live[db]:
			plan.Reappeared = append(plan.Reappeared, db)
		case len(in.Stored[db]) == 0:
			plan.Forgotten = append(plan.Forgotten, db)
		}
	}
	return plan
}

// PlanGlobalsRetention deletes globals dumps older than keep, always keeping the newest.
func PlanGlobalsRetention(objs []storage.Object, now time.Time, keep time.Duration) []string {
	sorted := append([]storage.Object(nil), objs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	var del []string
	for i, o := range sorted {
		if i != len(sorted)-1 && now.Sub(o.Modified) > keep {
			del = append(del, o.Key)
		}
	}
	return del
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
