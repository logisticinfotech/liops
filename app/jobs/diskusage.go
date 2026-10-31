// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"encoding/hex"
	"fmt"
	"strconv"
)

// DiskUsage measures what the databases take on disk, as monitoring series: each
// database's size ("db.size:<name>") and the write-ahead log ("disk.use:pg_wal") or the
// binlogs ("disk.use:binlogs"). The monitor can't read the database folders, so the
// root service asks the database and pushes the numbers to it.
func (r *Runner) DiskUsage(ctx context.Context) (map[string]float64, error) {
	if r.Cfg.IsMySQL() {
		return r.mysqlDiskUsage(ctx)
	}
	out := map[string]float64{}
	// Hex-encoded so that no database name can break the line/column parsing.
	rows, err := r.Cluster.Query(ctx, "postgres",
		"SELECT encode(convert_to(datname, 'UTF8'), 'hex'), pg_database_size(oid) FROM pg_database WHERE NOT datistemplate AND datallowconn")
	if err != nil {
		return nil, err
	}
	if err := addSizes(out, rows); err != nil {
		return nil, err
	}
	rows, err = r.Cluster.Query(ctx, "postgres", "SELECT coalesce(sum(size), 0) FROM pg_ls_waldir()")
	if err != nil {
		return nil, err
	}
	if err := addOne(out, "disk.use:pg_wal", rows); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Runner) mysqlDiskUsage(ctx context.Context) (map[string]float64, error) {
	out := map[string]float64{}
	db := r.XtraBackup.Instance
	// Data plus indexes, as InnoDB counts them (its files can be larger by the free space
	// inside them). stats_expiry 0: current sizes, not ones cached for a day.
	rows, err := db.Query(ctx, "", "SET SESSION information_schema_stats_expiry = 0; "+
		"SELECT HEX(table_schema), COALESCE(SUM(data_length + index_length), 0) FROM information_schema.tables "+
		"WHERE table_schema NOT IN ('information_schema', 'performance_schema') GROUP BY table_schema")
	if err != nil {
		return nil, err
	}
	if err := addSizes(out, rows); err != nil {
		return nil, err
	}
	rows, err = db.Query(ctx, "", "SHOW BINARY LOGS")
	if err != nil {
		return nil, err
	}
	var binlogs float64
	for _, row := range rows {
		if len(row) >= 2 {
			n, err := strconv.ParseFloat(row[1], 64)
			if err != nil {
				return nil, fmt.Errorf("binlog size %q: %w", row[1], err)
			}
			binlogs += n
		}
	}
	out["disk.use:binlogs"] = binlogs
	return out, nil
}

// addSizes reads rows of (hex name, bytes) into db.size: series.
func addSizes(out map[string]float64, rows [][]string) error {
	for _, row := range rows {
		if len(row) != 2 {
			return fmt.Errorf("database sizes: unexpected row %q", row)
		}
		name, err := hex.DecodeString(row[0])
		if err != nil {
			return fmt.Errorf("database sizes: %w", err)
		}
		n, err := strconv.ParseFloat(row[1], 64)
		if err != nil {
			return fmt.Errorf("database size %q: %w", row[1], err)
		}
		out["db.size:"+string(name)] = n
	}
	return nil
}

func addOne(out map[string]float64, name string, rows [][]string) error {
	if len(rows) != 1 || len(rows[0]) != 1 {
		return fmt.Errorf("%s: unexpected result %q", name, rows)
	}
	n, err := strconv.ParseFloat(rows[0][0], 64)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	out[name] = n
	return nil
}
