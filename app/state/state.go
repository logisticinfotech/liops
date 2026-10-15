// SPDX-License-Identifier: Apache-2.0

// Package state is pgtijori's own small database (bbolt): job history and the
// databases known to be deleted. It works when PostgreSQL is down.
//
// bbolt allows one process to have the file open at a time, and both the service
// and CLI commands (pgtijori run, status) need it, so every call opens the file,
// runs one transaction and closes it again. Writes are rare, so this costs nothing.
package state

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// DefaultPath is where the state file lives on a server.
const DefaultPath = "/var/lib/pgtijori/state.db"

var (
	bucketRuns    = []byte("runs")
	bucketDeleted = []byte("deleted")
	bucketUsers   = []byte("users")
	bucketAudit   = []byte("audit")
	bucketMeta    = []byte("meta")
)

// Run statuses.
const (
	StatusRunning     = "running"
	StatusOK          = "ok"
	StatusFailed      = "failed"
	StatusInterrupted = "interrupted" // pgtijori stopped while the job ran
)

// Run is one execution of a job.
type Run struct {
	ID      uint64    `json:"id"`
	Job     string    `json:"job"`
	Trigger string    `json:"trigger"` // "schedule" or "manual"
	Start   time.Time `json:"start"`
	End     time.Time `json:"end,omitzero"`
	Status  string    `json:"status"`
	Error   string    `json:"error,omitempty"` // one line
	Output  string    `json:"output,omitempty"`
	Size    int64     `json:"size,omitempty"` // bytes written to storage
	Dumps   []DumpRun `json:"dumps,omitempty"`
}

// DumpRun is the result for one database within a dumps run.
type DumpRun struct {
	DB       string        `json:"db"`
	Label    string        `json:"label,omitempty"`
	Key      string        `json:"key,omitempty"`
	Size     int64         `json:"size,omitempty"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}

// ErrNotFound is returned when a record doesn't exist.
var ErrNotFound = errors.New("not found")

// Store is the state file.
type Store struct {
	path string
}

// Open makes sure the state file can be opened, creating it if needed.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: path}
	err := s.update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketRuns, bucketDeleted, bucketUsers, bucketAudit, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) open(readOnly bool) (*bolt.DB, error) {
	db, err := bolt.Open(s.path, 0o600, &bolt.Options{Timeout: 30 * time.Second, ReadOnly: readOnly})
	if err != nil {
		return nil, fmt.Errorf("state %s: %w", s.path, err)
	}
	return db, nil
}

func (s *Store) update(fn func(*bolt.Tx) error) error {
	db, err := s.open(false)
	if err != nil {
		return err
	}
	return errors.Join(db.Update(fn), db.Close())
}

func (s *Store) view(fn func(*bolt.Tx) error) error {
	db, err := s.open(true)
	if err != nil {
		return err
	}
	return errors.Join(db.View(fn), db.Close())
}

func itob(n uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, n)
	return b
}

// StartRun records a new run with status running and sets its ID.
func (s *Store) StartRun(r *Run) error {
	r.Status = StatusRunning
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketRuns)
		id, err := b.NextSequence()
		if err != nil {
			return err
		}
		r.ID = id
		return putJSON(b, itob(id), r)
	})
}

// SaveRun stores a run again (e.g. when it finishes).
func (s *Store) SaveRun(r *Run) error {
	if r.ID == 0 {
		return errors.New("state: run has no ID")
	}
	return s.update(func(tx *bolt.Tx) error { return putJSON(tx.Bucket(bucketRuns), itob(r.ID), r) })
}

// MarkInterrupted marks every run still "running" as interrupted. Only call it while
// holding the job lock, when no job can be running.
func (s *Store) MarkInterrupted(at time.Time) (int, error) {
	n := 0
	err := s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketRuns)
		return b.ForEach(func(k, v []byte) error {
			var r Run
			if err := json.Unmarshal(v, &r); err != nil || r.Status != StatusRunning {
				return nil
			}
			r.Status, r.End, r.Error = StatusInterrupted, at, "pgtijori stopped before the job finished"
			n++
			return putJSON(b, k, &r)
		})
	})
	return n, err
}

// GetRun returns one run.
func (s *Store) GetRun(id uint64) (*Run, error) {
	var r *Run
	err := s.view(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketRuns).Get(itob(id))
		if v == nil {
			return ErrNotFound
		}
		r = &Run{}
		return json.Unmarshal(v, r)
	})
	return r, err
}

// Runs returns up to limit runs, newest first. An empty job means all jobs.
func (s *Store) Runs(job string, limit int) ([]Run, error) {
	var runs []Run
	err := s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketRuns).Cursor()
		for k, v := c.Last(); k != nil && len(runs) < limit; k, v = c.Prev() {
			var r Run
			if err := json.Unmarshal(v, &r); err != nil {
				continue
			}
			if job == "" || r.Job == job {
				runs = append(runs, r)
			}
		}
		return nil
	})
	return runs, err
}

// LastRun returns the newest run of job, optionally only with the given status.
func (s *Store) LastRun(job, status string) (*Run, error) {
	var found *Run
	err := s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketRuns).Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var r Run
			if err := json.Unmarshal(v, &r); err != nil {
				continue
			}
			if r.Job == job && (status == "" || r.Status == status) {
				found = &r
				return nil
			}
		}
		return nil
	})
	return found, err
}

// PruneRuns deletes runs that started before cutoff.
func (s *Store) PruneRuns(cutoff time.Time) (int, error) {
	var old [][]byte
	err := s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketRuns)
		c := b.Cursor()
		// IDs grow with start time, so stop at the first run that is new enough.
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var r Run
			if err := json.Unmarshal(v, &r); err == nil && !r.Start.Before(cutoff) {
				break
			}
			old = append(old, append([]byte(nil), k...))
		}
		// Deleting through the cursor while iterating can skip entries.
		for _, k := range old {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	return len(old), err
}

// Deleted returns the databases known to be gone, and since when.
func (s *Store) Deleted() (map[string]time.Time, error) {
	m := map[string]time.Time{}
	err := s.view(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketDeleted).ForEach(func(k, v []byte) error {
			var t time.Time
			if err := t.UnmarshalText(v); err == nil {
				m[string(k)] = t
			}
			return nil
		})
	})
	return m, err
}

// SetDeleted records that db has been gone since t.
func (s *Store) SetDeleted(db string, t time.Time) error {
	v, err := t.MarshalText()
	if err != nil {
		return err
	}
	return s.update(func(tx *bolt.Tx) error { return tx.Bucket(bucketDeleted).Put([]byte(db), v) })
}

// ClearDeleted forgets that db was gone.
func (s *Store) ClearDeleted(db string) error {
	return s.update(func(tx *bolt.Tx) error { return tx.Bucket(bucketDeleted).Delete([]byte(db)) })
}

func putJSON(b *bolt.Bucket, k []byte, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put(k, data)
}
