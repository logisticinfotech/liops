// SPDX-License-Identifier: Apache-2.0

package xtrabackup

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/logisticinfotech/liops/app/engines/dumps"
)

// errBinlogReset is when the current binlog's number is lower than the oldest backup's:
// RESET BINARY LOGS (or a renamed log_bin) started a new chain.
var errBinlogReset = errors.New("binlog numbers went backwards (RESET BINARY LOGS?)")

// BinlogPrefix is where binlogs are stored, gzipped. Current chains live under
// xtrabackup/binlog/<generation>/<file>.gz; a chain from before generations used
// xtrabackup/binlog/<file>.gz (Binlog.Gen empty).
const BinlogPrefix = "xtrabackup/binlog/"

// Binlog is one shipped binlog file.
type Binlog struct {
	Name string // binlog.000012
	Gen  string // empty: the legacy flat prefix
}

// Key is the object key of this binlog in storage.
func (b Binlog) Key() string {
	if b.Gen == "" {
		return BinlogPrefix + b.Name + ".gz"
	}
	return BinlogPrefix + b.Gen + "/" + b.Name + ".gz"
}

// ParseBinlogKey reads a Binlog from an object key under BinlogPrefix.
func ParseBinlogKey(key string) (Binlog, bool) {
	rest, ok := strings.CutPrefix(key, BinlogPrefix)
	if !ok {
		return Binlog{}, false
	}
	rest = strings.TrimSuffix(rest, ".gz")
	if rest == "" || rest == strings.TrimSuffix(key, ".gz") {
		return Binlog{}, false
	}
	if gen, name, found := strings.Cut(rest, "/"); found {
		if gen == "" || name == "" || strings.Contains(name, "/") {
			return Binlog{}, false
		}
		return Binlog{Name: name, Gen: gen}, true
	}
	return Binlog{Name: rest}, true
}

// BinlogStatus is the binlog MySQL is writing to now.
type BinlogStatus struct {
	File  string
	Pos   uint64
	GTIDs string
}

// Position is "file:position", e.g. binlog.000012:4711.
func (s BinlogStatus) Position() string { return s.File + ":" + strconv.FormatUint(s.Pos, 10) }

// BinlogStatus asks MySQL which binlog it is writing to, and how far.
func (e *Engine) BinlogStatus(ctx context.Context) (BinlogStatus, error) {
	rows, err := e.Instance.Query(ctx, "", "SHOW BINARY LOG STATUS")
	if err != nil {
		return BinlogStatus{}, err
	}
	if len(rows) != 1 || len(rows[0]) < 2 {
		return BinlogStatus{}, fmt.Errorf("SHOW BINARY LOG STATUS: unexpected output %q (is binary logging on?)", rows)
	}
	pos, err := strconv.ParseUint(rows[0][1], 10, 64)
	if err != nil {
		return BinlogStatus{}, fmt.Errorf("SHOW BINARY LOG STATUS: position %q", rows[0][1])
	}
	s := BinlogStatus{File: rows[0][0], Pos: pos}
	if len(rows[0]) >= 5 {
		s.GTIDs = strings.Join(strings.Fields(rows[0][4]), "")
	}
	return s, nil
}

// Rotate closes the current binlog and starts a new one, so the changes in it can be shipped.
func (e *Engine) Rotate(ctx context.Context) error {
	_, err := e.Instance.Query(ctx, "", "FLUSH BINARY LOGS")
	return err
}

// ShipResult is what a shipping run did.
type ShipResult struct {
	Shipped []string // binlogs uploaded now, oldest first
	Bytes   int64    // their size before compression
	// Gaps are binlogs a restore needs that are neither in storage nor on the server
	// any more: point-in-time restores across them are impossible.
	Gaps []string
	// NewGen: RESET BINARY LOGS (or lost GTIDs). Nothing was shipped; the next full
	// backup must start a new generation.
	NewGen bool
}

// ShipBinlogs uploads every closed binlog a restore of the current chain can need.
// lastSeen is the binlog name the previous run recorded; pendingNewGen is a sticky
// reset that has not yet been followed by a catch-up full. A reset (numbers went
// backwards against lastSeen or the newest backup, lost GTIDs, or a pending flag)
// ships nothing until that full starts a new generation.
func (e *Engine) ShipBinlogs(ctx context.Context, backups []Manifest, lastSeen string, pendingNewGen bool) (ShipResult, error) {
	var res ShipResult
	latest := Last(backups, "")
	if latest == nil {
		return res, nil
	}
	rows, err := e.Instance.Query(ctx, "", "SHOW BINARY LOGS")
	if err != nil {
		return res, err
	}
	if len(rows) == 0 {
		return res, fmt.Errorf("SHOW BINARY LOGS lists no binlogs (is binary logging on?)")
	}
	onServer := map[string]int64{}
	for _, r := range rows {
		if len(r) < 2 {
			return res, fmt.Errorf("SHOW BINARY LOGS: unexpected output %q", r)
		}
		size, err := strconv.ParseInt(r[1], 10, 64)
		if err != nil {
			return res, fmt.Errorf("SHOW BINARY LOGS: size %q", r[1])
		}
		onServer[r[0]] = size
	}
	current := rows[len(rows)-1][0] // still being written: shipped once it is closed

	if reset, why := e.binlogReset(ctx, latest, current, lastSeen, pendingNewGen); reset {
		res.NewGen = true
		res.Gaps = append(res.Gaps, why)
		return res, nil
	}

	oldest := oldestInGen(backups, latest.BinlogGen)
	names, err := binlogRange(oldest.BinlogFile, current)
	if err != nil {
		return res, err
	}

	stored, err := e.Store.List(ctx, BinlogPrefix)
	if err != nil {
		return res, err
	}
	shipped := map[string]bool{}
	for _, o := range stored {
		if b, ok := ParseBinlogKey(o.Key); ok && b.Gen == latest.BinlogGen {
			shipped[b.Name] = true
		}
	}

	var dir string
	for _, name := range names {
		if shipped[name] {
			continue
		}
		size, ok := onServer[name]
		if !ok {
			res.Gaps = append(res.Gaps, name)
			continue
		}
		if dir == "" {
			if dir, err = e.binlogDir(ctx); err != nil {
				return res, err
			}
		}
		b := Binlog{Name: name, Gen: latest.BinlogGen}
		if err := e.shipBinlog(ctx, filepath.Join(dir, name), size, b); err != nil {
			return res, fmt.Errorf("ship %s: %w", name, err)
		}
		res.Shipped = append(res.Shipped, name)
		res.Bytes += size
	}
	return res, nil
}

func oldestInGen(backups []Manifest, gen string) Manifest {
	var oldest Manifest
	for _, m := range backups {
		if m.BinlogGen != gen {
			continue
		}
		if oldest.BinlogFile == "" || binlogBefore(m.BinlogFile, oldest.BinlogFile) {
			oldest = m
		}
	}
	return oldest
}

// gtidSetRe is a GTID set from XtraBackup's manifest: UUIDs, ranges, commas.
var gtidSetRe = regexp.MustCompile(`^[0-9a-fA-F:,-]+$`)

// binlogReset reports whether the live server is a new binlog chain.
func (e *Engine) binlogReset(ctx context.Context, latest *Manifest, current, lastSeen string, pending bool) (bool, string) {
	if pending {
		return true, latest.BinlogFile
	}
	if lastSeen != "" && binlogBefore(current, lastSeen) {
		return true, lastSeen
	}
	if binlogBefore(current, latest.BinlogFile) {
		return true, latest.BinlogFile
	}
	if latest.GTIDs != "" {
		ok, err := e.gtidsStillHeld(ctx, latest.GTIDs)
		if err == nil && !ok {
			return true, latest.BinlogFile
		}
	}
	return false, ""
}

func (e *Engine) gtidsStillHeld(ctx context.Context, gtid string) (bool, error) {
	if !gtidSetRe.MatchString(gtid) {
		return true, nil
	}
	rows, err := e.Instance.Query(ctx, "", "SELECT GTID_SUBSET('"+gtid+"', @@GLOBAL.gtid_executed)")
	if err != nil {
		return false, err
	}
	if len(rows) != 1 || len(rows[0]) < 1 {
		return false, fmt.Errorf("GTID_SUBSET: unexpected output %q", rows)
	}
	return rows[0][0] == "1", nil
}

// binlogBefore reports whether a is a lower binlog number than b.
func binlogBefore(a, b string) bool {
	_, na, _, erra := splitBinlogName(a)
	_, nb, _, errb := splitBinlogName(b)
	if erra == nil && errb == nil {
		return na < nb
	}
	return a < b
}

// binlogDir is the directory MySQL writes its binlogs to.
func (e *Engine) binlogDir(ctx context.Context) (string, error) {
	base, err := e.Instance.Value(ctx, "SELECT @@log_bin_basename")
	if err != nil {
		return "", err
	}
	return filepath.Dir(base), nil
}

// shipBinlog uploads one closed binlog, gzipped, and checks that all of it was read.
func (e *Engine) shipBinlog(ctx context.Context, path string, size int64, b Binlog) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	key := b.Key()
	_, err = dumps.Upload(ctx, e.Store, key, func(ctx context.Context, w io.Writer) error {
		zw := gzip.NewWriter(w)
		n, err := io.Copy(zw, f)
		if err != nil {
			return err
		}
		if n != size {
			return fmt.Errorf("read %d bytes, but MySQL says the file has %d", n, size)
		}
		return zw.Close()
	})
	return err
}

// binlogRange lists the binlog names from first up to, not including, current: binlogs
// are numbered (binlog.000012), and the numbers keep counting across restarts.
func binlogRange(first, current string) ([]string, error) {
	base, from, width, err := splitBinlogName(first)
	if err != nil {
		return nil, err
	}
	base2, to, _, err := splitBinlogName(current)
	if err != nil {
		return nil, err
	}
	if base != base2 {
		return nil, fmt.Errorf("binlog names changed from %s to %s (log_bin was renamed?)", first, current)
	}
	if to < from {
		return nil, errBinlogReset
	}
	var names []string
	for n := from; n < to; n++ {
		names = append(names, fmt.Sprintf("%s.%0*d", base, width, n))
	}
	return names, nil
}

func splitBinlogName(name string) (base string, n, width int, err error) {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return "", 0, 0, fmt.Errorf("binlog name %q has no number", name)
	}
	n, err = strconv.Atoi(name[i+1:])
	if err != nil {
		return "", 0, 0, fmt.Errorf("binlog name %q has no number", name)
	}
	return name[:i], n, len(name) - i - 1, nil
}

// ShippedBinlogs returns the binlogs in storage, oldest first (by generation, then number).
func (e *Engine) ShippedBinlogs(ctx context.Context) ([]Binlog, error) {
	objs, err := e.Store.List(ctx, BinlogPrefix)
	if err != nil {
		return nil, err
	}
	var names []Binlog
	for _, o := range objs {
		b, ok := ParseBinlogKey(o.Key)
		if !ok {
			continue
		}
		names = append(names, b)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i].Gen != names[j].Gen {
			return names[i].Gen < names[j].Gen
		}
		return binlogBefore(names[i].Name, names[j].Name)
	})
	return names, nil
}
