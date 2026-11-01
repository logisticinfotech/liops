// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// How long each resolution is kept.
const (
	RawKeep    = time.Hour          // every sample, in memory
	MinuteKeep = 7 * 24 * time.Hour // 1-minute average and maximum, on disk
	HourKeep   = 90 * 24 * time.Hour
)

var (
	bucketSeries = []byte("series") // name → id
	bucketMinute = []byte("minute") // unix minute → record
	bucketHour   = []byte("hour")   // unix hour → record
)

// Store keeps the samples: the last hour in memory, then rolled up into 1-minute records
// for 7 days and hourly records for 90 days, each holding every series' average and
// maximum. Only the monitor process opens the file.
type Store struct {
	db *bolt.DB

	mu     sync.Mutex
	raw    []Sample
	ids    map[string]uint16
	names  map[uint16]string
	minute rollup
	hour   rollup

	shapeIDs map[string]uint32 // query shape key → id, loaded on first use

	routeIDs   map[routeKey]uint32 // request routes, loaded on first use
	routeNames map[uint32]routeKey
}

// rollup gathers the samples of one minute or hour.
type rollup struct {
	start    time.Time
	sum, max map[string]float64
	n        map[string]int
}

func newRollup(start time.Time) rollup {
	return rollup{start: start, sum: map[string]float64{}, max: map[string]float64{}, n: map[string]int{}}
}

func (r *rollup) add(name string, avg, mx float64) {
	r.sum[name] += avg
	r.n[name]++
	if old, ok := r.max[name]; !ok || mx > old {
		r.max[name] = mx
	}
}

func (r *rollup) empty() bool { return len(r.n) == 0 }

func OpenStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db, ids: map[string]uint16{}, names: map[uint16]string{}}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketSeries, bucketMinute, bucketHour, bucketShapes, bucketQueries, bucketBlobs,
			bucketRoutes, bucketReqMinute, bucketReqHour, bucketReqSlow, bucketReqClients} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketSeries).ForEach(func(k, v []byte) error {
			id := binary.BigEndian.Uint16(v)
			s.ids[string(k)], s.names[id] = id, string(k)
			return nil
		})
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Add records a sample. When it starts a new minute (or hour), the finished one is written.
func (s *Store) Add(sample Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.raw = append(s.raw, sample)
	cut := 0
	for cut < len(s.raw) && sample.Time.Sub(s.raw[cut].Time) > RawKeep {
		cut++
	}
	s.raw = s.raw[cut:]

	minute := sample.Time.Truncate(time.Minute)
	var err error
	if !s.minute.start.Equal(minute) {
		if !s.minute.empty() {
			err = s.flushMinuteLocked()
		}
		s.minute = newRollup(minute)
	}
	for name, v := range sample.Values {
		s.minute.add(name, v, v)
	}
	return err
}

// flushMinuteLocked writes the finished minute and folds it into the hour, writing the
// hour when it's finished too.
func (s *Store) flushMinuteLocked() error {
	m := s.minute
	err := s.writeLocked(bucketMinute, uint32(m.start.Unix()/60), m)
	hour := m.start.Truncate(time.Hour)
	if !s.hour.start.Equal(hour) {
		if !s.hour.empty() {
			if herr := s.writeLocked(bucketHour, uint32(s.hour.start.Unix()/3600), s.hour); err == nil {
				err = herr
			}
		}
		s.hour = newRollup(hour)
	}
	for name, n := range m.n {
		s.hour.add(name, m.sum[name]/float64(n), m.max[name])
	}
	return err
}

// writeLocked stores a rollup as its averages and maxima: id (2 bytes), average and
// maximum (4 bytes each) per series.
func (s *Store) writeLocked(bucket []byte, key uint32, r rollup) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		series := tx.Bucket(bucketSeries)
		rec := make([]byte, 0, len(r.n)*10)
		for name, n := range r.n {
			id, ok := s.ids[name]
			if !ok {
				if len(s.ids) >= math.MaxUint16 {
					continue
				}
				id = uint16(len(s.ids) + 1)
				if err := series.Put([]byte(name), binary.BigEndian.AppendUint16(nil, id)); err != nil {
					return err
				}
				s.ids[name], s.names[id] = id, name
			}
			rec = binary.BigEndian.AppendUint16(rec, id)
			rec = binary.BigEndian.AppendUint32(rec, math.Float32bits(float32(r.sum[name]/float64(n))))
			rec = binary.BigEndian.AppendUint32(rec, math.Float32bits(float32(r.max[name])))
		}
		return tx.Bucket(bucket).Put(binary.BigEndian.AppendUint32(nil, key), rec)
	})
}

// Prune deletes 1-minute records older than MinuteKeep and hourly ones older than HourKeep.
func (s *Store) Prune(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, p := range []struct {
			bucket []byte
			key    uint32
		}{
			{bucketMinute, uint32(now.Add(-MinuteKeep).Unix() / 60)},
			{bucketHour, uint32(now.Add(-HourKeep).Unix() / 3600)},
		} {
			c := tx.Bucket(p.bucket).Cursor()
			for k, _ := c.First(); k != nil && binary.BigEndian.Uint32(k) < p.key; k, _ = c.Next() {
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		if err := pruneQueries(tx, now); err != nil {
			return err
		}
		if err := pruneRequests(tx, now); err != nil {
			return err
		}
		s.routeIDs, s.routeNames = nil, nil // reload after unused ids were dropped
		return nil
	})
}

// Series is the result of a query: X as Unix seconds and, per series, its average (and
// maximum) at each X; nil where there's no value.
type Series struct {
	Step   time.Duration // 15 s, 1 minute or 1 hour
	X      []int64
	Values map[string][]*float64
	Max    map[string][]*float64
}

// Query returns the named series between from and to, at the resolution the range calls
// for: every sample for up to an hour, 1-minute records up to 7 days, hourly beyond.
func (s *Store) Query(names []string, from, to time.Time) (Series, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Series{Values: map[string][]*float64{}, Max: map[string][]*float64{}}
	for _, n := range names {
		out.Values[n], out.Max[n] = nil, nil
	}
	appendPoint := func(at int64, avg, mx map[string]float64) {
		out.X = append(out.X, at)
		for _, n := range names {
			var a, m *float64
			if v, ok := avg[n]; ok {
				a = &v
			}
			if v, ok := mx[n]; ok {
				m = &v
			}
			out.Values[n] = append(out.Values[n], a)
			out.Max[n] = append(out.Max[n], m)
		}
	}

	span := to.Sub(from)
	switch {
	case span <= RawKeep:
		out.Step = 15 * time.Second
		for _, smp := range s.raw {
			if !smp.Time.Before(from) && !smp.Time.After(to) {
				appendPoint(smp.Time.Unix(), smp.Values, smp.Values)
			}
		}
		return out, nil
	case span <= MinuteKeep:
		out.Step = time.Minute
		return out, s.scanLocked(bucketMinute, 60, from, to, appendPoint)
	default:
		out.Step = time.Hour
		return out, s.scanLocked(bucketHour, 3600, from, to, appendPoint)
	}
}

func (s *Store) scanLocked(bucket []byte, unit int64, from, to time.Time, appendPoint func(int64, map[string]float64, map[string]float64)) error {
	return s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucket).Cursor()
		start := binary.BigEndian.AppendUint32(nil, uint32(from.Unix()/unit))
		end := uint32(to.Unix() / unit)
		for k, v := c.Seek(start); k != nil && binary.BigEndian.Uint32(k) <= end; k, v = c.Next() {
			avg, mx := map[string]float64{}, map[string]float64{}
			for i := 0; i+10 <= len(v); i += 10 {
				name, ok := s.names[binary.BigEndian.Uint16(v[i:])]
				if !ok {
					continue
				}
				avg[name] = float64(math.Float32frombits(binary.BigEndian.Uint32(v[i+2:])))
				mx[name] = float64(math.Float32frombits(binary.BigEndian.Uint32(v[i+6:])))
			}
			appendPoint(int64(binary.BigEndian.Uint32(k))*unit, avg, mx)
		}
		return nil
	})
}

// Latest returns the newest sample, or false before the first.
func (s *Store) Latest() (Sample, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.raw) == 0 {
		return Sample{}, false
	}
	return s.raw[len(s.raw)-1], true
}

// Names returns every series name seen in the last hour, or ever stored, sorted.
func (s *Store) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for n := range s.ids {
		seen[n] = true
	}
	for _, smp := range s.raw {
		for n := range smp.Values {
			seen[n] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
