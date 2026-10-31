// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// QueryInterval is how often the databases' statement statistics are read.
const QueryInterval = 5 * time.Minute

// maxQueryRows bounds one snapshot: the query shapes with the most time are kept.
const maxQueryRows = 500

var (
	bucketShapes  = []byte("shapes")  // id → QueryShape (JSON)
	bucketQueries = []byte("queries") // unix/300 → the snapshot's rows
	bucketBlobs   = []byte("blobs")   // name → JSON (the latest table snapshot, …)
)

// QueryShape is one normalised statement in one database: literals are ? (MySQL) or $1
// (PostgreSQL), so it holds no values.
type QueryShape struct {
	Server    string    `json:"server"` // DBServer.Label
	DB        string    `json:"db"`
	ID        string    `json:"id"` // MySQL digest, PostgreSQL queryid
	Text      string    `json:"text"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	MaxMS     float64   `json:"max_ms"` // the slowest call since the statistics were reset
}

func (q QueryShape) key() string { return q.Server + "\x00" + q.DB + "\x00" + q.ID }

// QueryCounters are a query shape's numbers over a period.
type QueryCounters struct {
	Calls  float64 `json:"calls"`
	TimeMS float64 `json:"time_ms"`
	// RowsExamined is MySQL's rows examined; for PostgreSQL, the blocks (8 KB pages) read,
	// from memory or disk: there is no row count of what a query looked at.
	RowsExamined float64 `json:"rows_examined"`
	RowsSent     float64 `json:"rows_sent"`
	NoIndex      float64 `json:"no_index"`  // MySQL: calls that used no index, or no good one
	TempDisk     float64 `json:"temp_disk"` // MySQL: temporary tables on disk; PostgreSQL: temp blocks written
}

func (c *QueryCounters) add(o QueryCounters) {
	c.Calls += o.Calls
	c.TimeMS += o.TimeMS
	c.RowsExamined += o.RowsExamined
	c.RowsSent += o.RowsSent
	c.NoIndex += o.NoIndex
	c.TempDisk += o.TempDisk
}

// QueryTotal is a query shape with its numbers over a range.
type QueryTotal struct {
	QueryShape
	QueryCounters
}

// shapeIDs loads the shape ids. The caller holds s.mu.
func (s *Store) shapeIDsLocked() error {
	if s.shapeIDs != nil {
		return nil
	}
	s.shapeIDs = map[string]uint32{}
	return s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketShapes).ForEach(func(k, v []byte) error {
			var q QueryShape
			if json.Unmarshal(v, &q) == nil {
				s.shapeIDs[q.key()] = binary.BigEndian.Uint32(k)
			}
			return nil
		})
	})
}

// AddQueries records one statement snapshot: each shape's numbers since the last one.
// Shapes are saved (or updated) alongside.
func (s *Store) AddQueries(at time.Time, rows []QueryTotal) error {
	sortByTime(rows)
	if len(rows) > maxQueryRows {
		rows = rows[:maxQueryRows]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.shapeIDsLocked(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		shapes := tx.Bucket(bucketShapes)
		var rec []byte
		for _, r := range rows {
			k := r.key()
			id, ok := s.shapeIDs[k]
			if !ok {
				next, err := shapes.NextSequence()
				if err != nil {
					return err
				}
				id = uint32(next)
				s.shapeIDs[k] = id
			}
			idKey := binary.BigEndian.AppendUint32(nil, id)
			shape := r.QueryShape
			if old := shapes.Get(idKey); old != nil {
				var prev QueryShape
				if json.Unmarshal(old, &prev) == nil {
					if !prev.FirstSeen.IsZero() && (shape.FirstSeen.IsZero() || prev.FirstSeen.Before(shape.FirstSeen)) {
						shape.FirstSeen = prev.FirstSeen
					}
					if shape.Text == "" {
						shape.Text = prev.Text // the text is only read the first time
					}
				}
			}
			if shape.FirstSeen.IsZero() {
				shape.FirstSeen = at
			}
			shape.LastSeen = at
			data, err := json.Marshal(shape)
			if err != nil {
				return err
			}
			if err := shapes.Put(idKey, data); err != nil {
				return err
			}
			rec = binary.AppendUvarint(rec, uint64(id))
			for _, v := range []float64{r.Calls, r.TimeMS * 1000, r.RowsExamined, r.RowsSent, r.NoIndex, r.TempDisk} {
				rec = binary.AppendUvarint(rec, uint64(math.Max(0, math.Round(v))))
			}
		}
		return tx.Bucket(bucketQueries).Put(binary.BigEndian.AppendUint32(nil, uint32(at.Unix()/300)), rec)
	})
}

// KnownShapes reports which of the rows' shapes are stored already (with their text).
func (s *Store) KnownShapes(rows []QueryTotal) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	if s.shapeIDsLocked() != nil {
		return out
	}
	for _, r := range rows {
		_, out[r.key()] = s.shapeIDs[r.key()]
	}
	return out
}

func sortByTime(rows []QueryTotal) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].TimeMS > rows[j].TimeMS })
}

// scanQueries calls fn for every row recorded between from and to.
func (s *Store) scanQueries(tx *bolt.Tx, from, to time.Time, fn func(at int64, id uint32, c QueryCounters)) {
	c := tx.Bucket(bucketQueries).Cursor()
	end := uint32(to.Unix() / 300)
	for k, v := c.Seek(binary.BigEndian.AppendUint32(nil, uint32(from.Unix()/300))); k != nil && binary.BigEndian.Uint32(k) <= end; k, v = c.Next() {
		at := int64(binary.BigEndian.Uint32(k)) * 300
		for len(v) > 0 {
			var nums [7]uint64
			for i := range nums {
				n, size := binary.Uvarint(v)
				if size <= 0 {
					return // a damaged record: skip the rest
				}
				nums[i], v = n, v[size:]
			}
			fn(at, uint32(nums[0]), QueryCounters{Calls: float64(nums[1]), TimeMS: float64(nums[2]) / 1000,
				RowsExamined: float64(nums[3]), RowsSent: float64(nums[4]), NoIndex: float64(nums[5]), TempDisk: float64(nums[6])})
		}
	}
}

// TopQueries returns the query shapes with the most total time between from and to, for
// one server ("" for all) and one database ("" for all).
func (s *Store) TopQueries(from, to time.Time, server, db string, limit int) ([]QueryTotal, error) {
	totals := map[uint32]*QueryTotal{}
	err := s.db.View(func(tx *bolt.Tx) error {
		s.scanQueries(tx, from, to, func(_ int64, id uint32, c QueryCounters) {
			t, ok := totals[id]
			if !ok {
				t = &QueryTotal{}
				totals[id] = t
			}
			t.QueryCounters.add(c)
		})
		shapes := tx.Bucket(bucketShapes)
		for id, t := range totals {
			data := shapes.Get(binary.BigEndian.AppendUint32(nil, id))
			if data == nil || json.Unmarshal(data, &t.QueryShape) != nil ||
				(server != "" && t.Server != server) || (db != "" && t.DB != db) {
				delete(totals, id)
			}
		}
		return nil
	})
	out := make([]QueryTotal, 0, len(totals))
	for _, t := range totals {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeMS > out[j].TimeMS })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

// QueryPoint is one snapshot of a query shape's numbers.
type QueryPoint struct {
	At int64 `json:"at"`
	QueryCounters
}

// QueryDetail returns one shape and its snapshots between from and to.
func (s *Store) QueryDetail(server, db, id string, from, to time.Time) (QueryShape, []QueryPoint, bool, error) {
	s.mu.Lock()
	err := s.shapeIDsLocked()
	sid, ok := s.shapeIDs[QueryShape{Server: server, DB: db, ID: id}.key()]
	s.mu.Unlock()
	if err != nil || !ok {
		return QueryShape{}, nil, false, err
	}
	var shape QueryShape
	var points []QueryPoint
	err = s.db.View(func(tx *bolt.Tx) error {
		if data := tx.Bucket(bucketShapes).Get(binary.BigEndian.AppendUint32(nil, sid)); data != nil {
			if err := json.Unmarshal(data, &shape); err != nil {
				return err
			}
		}
		s.scanQueries(tx, from, to, func(at int64, id uint32, c QueryCounters) {
			if id == sid {
				points = append(points, QueryPoint{At: at, QueryCounters: c})
			}
		})
		return nil
	})
	return shape, points, true, err
}

// pruneQueries deletes snapshots older than MinuteKeep.
func pruneQueries(tx *bolt.Tx, now time.Time) error {
	c := tx.Bucket(bucketQueries).Cursor()
	cut := uint32(now.Add(-MinuteKeep).Unix() / 300)
	for k, _ := c.First(); k != nil && binary.BigEndian.Uint32(k) < cut; k, _ = c.Next() {
		if err := c.Delete(); err != nil {
			return err
		}
	}
	return nil
}

// SetBlob saves a value as JSON under name.
func (s *Store) SetBlob(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketBlobs).Put([]byte(name), data) })
}

// Blob loads the value saved under name; false if there is none.
func (s *Store) Blob(name string, v any) (bool, error) {
	var data []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		data = append([]byte(nil), tx.Bucket(bucketBlobs).Get([]byte(name))...)
		return nil
	})
	if err != nil || len(data) == 0 {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}
