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
	"sort"
	"strconv"
	"strings"

	"github.com/logisticinfotech/liops/app/engines/dumps"
)

// errBinlogReset is when the current binlog's number is lower than the oldest backup's:
// RESET BINARY LOGS (or a renamed log_bin) started a new chain.
var errBinlogReset = errors.New("binlog numbers went backwards (RESET BINARY LOGS?)")

// BinlogPrefix is where binlogs are stored, gzipped: xtrabackup/binlog/<file>.gz.
const BinlogPrefix = "xtrabackup/binlog/"

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
}

// ShipBinlogs uploads every closed binlog from oldestNeeded on (the binlog the oldest
// kept backup starts in) that isn't in storage yet. Older binlogs can't be replayed
// onto any backup, so they aren't shipped; with no backup yet (oldestNeeded empty),
// nothing is.
func (e *Engine) ShipBinlogs(ctx context.Context, oldestNeeded string) (ShipResult, error) {
	var res ShipResult
	if oldestNeeded == "" {
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

	stored, err := e.Store.List(ctx, BinlogPrefix)
	if err != nil {
		return res, err
	}
	shipped := map[string]bool{}
	for _, o := range stored {
		shipped[strings.TrimSuffix(strings.TrimPrefix(o.Key, BinlogPrefix), ".gz")] = true
	}

	names, err := binlogRange(oldestNeeded, current)
	if errors.Is(err, errBinlogReset) {
		// The live chain is a new sequence: don't invent names from the old number up,
		// ship what is on the server, and report the old starting file as a gap so a
		// catch-up full can start a restore chain that uses the new binlogs.
		res.Gaps = append(res.Gaps, oldestNeeded)
		for _, r := range rows {
			if r[0] != current {
				names = append(names, r[0])
			}
		}
	} else if err != nil {
		return res, err
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
		if err := e.shipBinlog(ctx, filepath.Join(dir, name), size); err != nil {
			return res, fmt.Errorf("ship %s: %w", name, err)
		}
		res.Shipped = append(res.Shipped, name)
		res.Bytes += size
	}
	return res, nil
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
func (e *Engine) shipBinlog(ctx context.Context, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	key := BinlogPrefix + filepath.Base(path) + ".gz"
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

// ShippedBinlogs returns the binlogs in storage, oldest first.
func (e *Engine) ShippedBinlogs(ctx context.Context) ([]string, error) {
	objs, err := e.Store.List(ctx, BinlogPrefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, o := range objs {
		names = append(names, strings.TrimSuffix(strings.TrimPrefix(o.Key, BinlogPrefix), ".gz"))
	}
	sort.Strings(names) // zero-padded numbers sort in order
	return names, nil
}
