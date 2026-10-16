// SPDX-License-Identifier: Apache-2.0

package state

import (
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
)

// AuditEntry records who did what in the UI.
type AuditEntry struct {
	ID     uint64    `json:"id"`
	Time   time.Time `json:"time"`
	User   string    `json:"user"`             // "ui" or "root (CLI)"
	Action string    `json:"action"`           // e.g. "download", "settings", "run"
	Detail string    `json:"detail,omitempty"` // e.g. the downloaded key
	OK     bool      `json:"ok"`
}

// Audit appends an entry.
func (s *Store) Audit(e AuditEntry) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAudit)
		id, err := b.NextSequence()
		if err != nil {
			return err
		}
		e.ID = id
		return putJSON(b, itob(id), e)
	})
}

// AuditLog returns up to limit entries older than before (0 for the newest), newest first.
func (s *Store) AuditLog(before uint64, limit int) ([]AuditEntry, error) {
	var entries []AuditEntry
	err := s.view(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketAudit).Cursor()
		var k, v []byte
		if before == 0 {
			k, v = c.Last()
		} else {
			c.Seek(itob(before))
			k, v = c.Prev()
		}
		for ; k != nil && len(entries) < limit; k, v = c.Prev() {
			var e AuditEntry
			if err := json.Unmarshal(v, &e); err == nil {
				entries = append(entries, e)
			}
		}
		return nil
	})
	return entries, err
}

// PruneAudit deletes entries older than cutoff.
func (s *Store) PruneAudit(cutoff time.Time) (int, error) {
	var old [][]byte
	err := s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAudit)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var e AuditEntry
			if err := json.Unmarshal(v, &e); err == nil && !e.Time.Before(cutoff) {
				break
			}
			old = append(old, append([]byte(nil), k...))
		}
		for _, k := range old {
			if err := b.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	return len(old), err
}

// GetMeta returns a small stored value, or "" if unset.
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.view(func(tx *bolt.Tx) error {
		v = string(tx.Bucket(bucketMeta).Get([]byte(key)))
		return nil
	})
	return v, err
}

// SetMeta stores a small value; "" deletes it.
func (s *Store) SetMeta(key, value string) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		if value == "" {
			return b.Delete([]byte(key))
		}
		return b.Put([]byte(key), []byte(value))
	})
}
