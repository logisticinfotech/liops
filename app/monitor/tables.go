// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// TableInterval is how often tables and indexes are read.
const TableInterval = time.Hour

// maxDatabases caps the PostgreSQL databases read per pass (each needs its own connection).
const maxDatabases = 300

// DBTables is one server's tables and indexes, read hourly.
type DBTables struct {
	Server  string      `json:"server"`
	At      time.Time   `json:"at"`
	Tables  []TableInfo `json:"tables"`
	Indexes []IndexInfo `json:"indexes"`
	// Memory: the settings that decide how much data stays in memory, in bytes
	// (innodb_buffer_pool_size; shared_buffers, effective_cache_size, work_mem).
	Memory map[string]float64 `json:"memory"`
	// XIDAge is each PostgreSQL database's transaction ID age: at about 2 billion
	// PostgreSQL stops accepting writes until it's vacuumed.
	XIDAge map[string]float64 `json:"xid_age,omitempty"`
	// StatsSince is when the index use counts started (server start or stats reset).
	StatsSince time.Time `json:"stats_since"`
	Problems   []string  `json:"problems,omitempty"` // databases or lists it couldn't read
	// Growth is each table's growth in bytes over about a day, when known.
	Growth map[string]float64 `json:"growth,omitempty"`
}

// TableInfo is one table.
type TableInfo struct {
	DB         string  `json:"db"`
	Schema     string  `json:"schema,omitempty"` // PostgreSQL
	Name       string  `json:"name"`
	Rows       float64 `json:"rows"` // an estimate on both databases
	Bytes      float64 `json:"bytes"`
	PrimaryKey bool    `json:"primary_key"`
	// PostgreSQL only: how it has been read since the statistics started.
	SeqScans    float64   `json:"seq_scans,omitempty"`
	SeqRowsRead float64   `json:"seq_rows_read,omitempty"`
	IdxScans    float64   `json:"idx_scans,omitempty"`
	DeadRows    float64   `json:"dead_rows,omitempty"`
	LastVacuum  time.Time `json:"last_vacuum,omitzero"`
}

// Key identifies a table within its server: db/schema.name or db/name.
func (t TableInfo) Key() string {
	if t.Schema != "" {
		return t.DB + "/" + t.Schema + "." + t.Name
	}
	return t.DB + "/" + t.Name
}

// IndexInfo is one index.
type IndexInfo struct {
	DB         string `json:"db"`
	Schema     string `json:"schema,omitempty"`
	Table      string `json:"table"`
	Name       string `json:"name"`
	Definition string `json:"definition"` // its columns, e.g. "(email)" or PostgreSQL's CREATE INDEX
	Unique     bool   `json:"unique"`
	Primary    bool   `json:"primary"`
	Unused     bool   `json:"unused"`              // no reads since the statistics started
	Duplicate  string `json:"duplicate,omitempty"` // the index that makes it redundant
}

// TableKey is the key of the index's table (TableInfo.Key).
func (i IndexInfo) TableKey() string {
	return TableInfo{DB: i.DB, Schema: i.Schema, Name: i.Table}.Key()
}

var mysqlSystemSchemas = "('mysql', 'sys', 'performance_schema', 'information_schema')"

func readMySQLTables(ctx context.Context, d DBServer) (DBTables, error) {
	t := DBTables{Server: d.Label(), Memory: map[string]float64{}}
	rows, err := d.Query(ctx, "", `SET SESSION information_schema_stats_expiry = 0;
SELECT HEX(TABLE_SCHEMA), HEX(TABLE_NAME), IFNULL(TABLE_ROWS, 0), IFNULL(DATA_LENGTH + INDEX_LENGTH, 0)
FROM information_schema.TABLES WHERE TABLE_TYPE = 'BASE TABLE' AND TABLE_SCHEMA NOT IN `+mysqlSystemSchemas)
	if err != nil {
		return t, err
	}
	for _, r := range rows {
		if len(r) == 4 {
			t.Tables = append(t.Tables, TableInfo{DB: unhex(r[0]), Name: unhex(r[1]), Rows: atof(r[2]), Bytes: atof(r[3])})
		}
	}
	rows, err = d.Query(ctx, "", `SELECT HEX(TABLE_SCHEMA), HEX(TABLE_NAME), HEX(INDEX_NAME), NON_UNIQUE, HEX(IFNULL(COLUMN_NAME, EXPRESSION))
FROM information_schema.STATISTICS WHERE TABLE_SCHEMA NOT IN `+mysqlSystemSchemas+` ORDER BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`)
	if err != nil {
		return t, err
	}
	byKey := map[string]int{}
	for _, r := range rows {
		if len(r) != 5 {
			continue
		}
		ix := IndexInfo{DB: unhex(r[0]), Table: unhex(r[1]), Name: unhex(r[2]), Unique: r[3] == "0"}
		ix.Primary = ix.Name == "PRIMARY"
		k := ix.TableKey() + "/" + ix.Name
		if i, ok := byKey[k]; ok {
			t.Indexes[i].Definition = strings.TrimSuffix(t.Indexes[i].Definition, ")") + ", " + unhex(r[4]) + ")"
			continue
		}
		ix.Definition = "(" + unhex(r[4]) + ")"
		byKey[k] = len(t.Indexes)
		t.Indexes = append(t.Indexes, ix)
	}
	// Unused and redundant indexes, from the sys schema. Not fatal: they need
	// performance_schema's index statistics.
	if rows, err := d.Query(ctx, "", "SELECT HEX(object_schema), HEX(object_name), HEX(index_name) FROM sys.schema_unused_indexes"); err == nil {
		for _, r := range rows {
			if len(r) == 3 {
				if i, ok := byKey[TableInfo{DB: unhex(r[0]), Name: unhex(r[1])}.Key()+"/"+unhex(r[2])]; ok {
					t.Indexes[i].Unused = true
				}
			}
		}
	} else {
		t.Problems = append(t.Problems, "unused indexes: "+err.Error())
	}
	if rows, err := d.Query(ctx, "", "SELECT HEX(table_schema), HEX(table_name), HEX(redundant_index_name), HEX(dominant_index_name) FROM sys.schema_redundant_indexes"); err == nil {
		for _, r := range rows {
			if len(r) == 4 {
				if i, ok := byKey[TableInfo{DB: unhex(r[0]), Name: unhex(r[1])}.Key()+"/"+unhex(r[2])]; ok {
					t.Indexes[i].Duplicate = unhex(r[3])
				}
			}
		}
	} else {
		t.Problems = append(t.Problems, "redundant indexes: "+err.Error())
	}
	rows, err = d.Query(ctx, "", "SELECT 'innodb_buffer_pool_size', @@innodb_buffer_pool_size UNION ALL SELECT 'uptime', VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Uptime'")
	if err != nil {
		return t, err
	}
	for _, r := range rows {
		if len(r) != 2 {
			continue
		}
		if r[0] == "uptime" {
			t.StatsSince = time.Now().Add(-time.Duration(atof(r[1])) * time.Second).Truncate(time.Second)
		} else {
			t.Memory[r[0]] = atof(r[1])
		}
	}
	markPrimaryKeys(&t)
	return t, nil
}

func readPostgresTables(ctx context.Context, d DBServer) (DBTables, error) {
	t := DBTables{Server: d.Label(), Memory: map[string]float64{}, XIDAge: map[string]float64{}}
	rows, err := d.Query(ctx, "", `SELECT encode(convert_to(datname, 'UTF8'), 'hex'), age(datfrozenxid),
coalesce(extract(epoch FROM (SELECT min(stats_reset) FROM pg_stat_database)), extract(epoch FROM pg_postmaster_start_time()))
FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY datname`)
	if err != nil {
		return t, err
	}
	var dbs []string
	for _, r := range rows {
		if len(r) == 3 {
			db := unhex(r[0])
			dbs = append(dbs, db)
			t.XIDAge[db] = atof(r[1])
			t.StatsSince = time.Unix(int64(atof(r[2])), 0)
		}
	}
	if len(dbs) > maxDatabases {
		t.Problems = append(t.Problems, fmt.Sprintf("%d databases: only the first %d are read", len(dbs), maxDatabases))
		dbs = dbs[:maxDatabases]
	}
	for _, db := range dbs {
		if err := readPostgresDB(ctx, d, db, &t); err != nil {
			t.Problems = append(t.Problems, db+": "+err.Error())
		}
	}
	rows, err = d.Query(ctx, "", `SELECT name, pg_size_bytes(current_setting(name))
FROM (VALUES ('shared_buffers'), ('effective_cache_size'), ('work_mem'), ('maintenance_work_mem')) v(name)`)
	if err != nil {
		return t, err
	}
	for _, r := range rows {
		if len(r) == 2 {
			t.Memory[r[0]] = atof(r[1])
		}
	}
	markPrimaryKeys(&t)
	return t, nil
}

func readPostgresDB(ctx context.Context, d DBServer, db string, t *DBTables) error {
	hex := func(col string) string { return "encode(convert_to(" + col + ", 'UTF8'), 'hex')" }
	rows, err := d.Query(ctx, db, `SELECT `+hex("schemaname")+`, `+hex("relname")+`, n_live_tup, pg_total_relation_size(relid),
coalesce(seq_scan, 0), coalesce(seq_tup_read, 0), coalesce(idx_scan, 0), n_dead_tup,
coalesce(extract(epoch FROM greatest(last_autovacuum, last_vacuum)), 0)
FROM pg_stat_user_tables`)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if len(r) != 9 {
			continue
		}
		ti := TableInfo{DB: db, Schema: unhex(r[0]), Name: unhex(r[1]), Rows: atof(r[2]), Bytes: atof(r[3]),
			SeqScans: atof(r[4]), SeqRowsRead: atof(r[5]), IdxScans: atof(r[6]), DeadRows: atof(r[7])}
		if v := atof(r[8]); v > 0 {
			ti.LastVacuum = time.Unix(int64(v), 0)
		}
		t.Tables = append(t.Tables, ti)
	}
	rows, err = d.Query(ctx, db, `SELECT `+hex("s.schemaname")+`, `+hex("s.relname")+`, `+hex("s.indexrelname")+`,
x.indisunique, x.indisprimary, coalesce(s.idx_scan, 0), `+hex("pg_get_indexdef(s.indexrelid)")+`,
`+hex("x.indkey::text || ' ' || coalesce(pg_get_expr(x.indexprs, x.indrelid), '') || ' ' || coalesce(pg_get_expr(x.indpred, x.indrelid), '')")+`
FROM pg_stat_user_indexes s JOIN pg_index x ON x.indexrelid = s.indexrelid ORDER BY s.indexrelname`)
	if err != nil {
		return err
	}
	seen := map[string]string{} // table + columns → first index with them
	for _, r := range rows {
		if len(r) != 8 {
			continue
		}
		ix := IndexInfo{DB: db, Schema: unhex(r[0]), Table: unhex(r[1]), Name: unhex(r[2]), Unique: r[3] == "t", Primary: r[4] == "t",
			Definition: unhex(r[6])}
		ix.Unused = atof(r[5]) == 0 && !ix.Unique && !ix.Primary
		same := ix.TableKey() + "|" + unhex(r[7])
		if first, ok := seen[same]; ok {
			ix.Duplicate = first
		} else {
			seen[same] = ix.Name
		}
		t.Indexes = append(t.Indexes, ix)
	}
	return nil
}

// markPrimaryKeys sets TableInfo.PrimaryKey from the indexes.
func markPrimaryKeys(t *DBTables) {
	pk := map[string]bool{}
	for _, ix := range t.Indexes {
		if ix.Primary {
			pk[ix.TableKey()] = true
		}
	}
	for i := range t.Tables {
		t.Tables[i].PrimaryKey = pk[t.Tables[i].Key()]
	}
}

// tableSizes is one day's table sizes, kept to work out growth.
type tableSizes struct {
	At    time.Time          `json:"at"`
	Bytes map[string]float64 `json:"bytes"`
}

// readTables reads a server's tables and indexes, works out each table's growth since
// about a day ago, and saves the snapshot.
func (m *Monitor) readTables(ctx context.Context, d DBServer) error {
	var t DBTables
	var err error
	if d.Engine == EngineMySQL {
		t, err = readMySQLTables(ctx, d)
	} else {
		t, err = readPostgresTables(ctx, d)
	}
	if err != nil {
		return err
	}
	t.At = m.Now()
	sort.Slice(t.Tables, func(i, j int) bool { return t.Tables[i].Bytes > t.Tables[j].Bytes })

	// One size snapshot a day, eight days back.
	var days []tableSizes
	m.Store.Blob("table-days:"+d.Label(), &days)
	now := tableSizes{At: t.At, Bytes: map[string]float64{}}
	for _, ti := range t.Tables {
		now.Bytes[ti.Key()] = ti.Bytes
	}
	if len(days) == 0 || t.At.Sub(days[len(days)-1].At) >= 23*time.Hour {
		days = append(days, now)
		if len(days) > 8 {
			days = days[len(days)-8:]
		}
		if err := m.Store.SetBlob("table-days:"+d.Label(), days); err != nil {
			return err
		}
	}
	// Growth against the newest snapshot at least 20 hours old, scaled to a day.
	for i := len(days) - 1; i >= 0; i-- {
		if span := t.At.Sub(days[i].At); span >= 20*time.Hour {
			t.Growth = map[string]float64{}
			for k, b := range now.Bytes {
				if old, ok := days[i].Bytes[k]; ok {
					t.Growth[k] = (b - old) / span.Hours() * 24
				}
			}
			break
		}
	}
	return m.Store.SetBlob("tables:"+d.Label(), t)
}

// Tables returns a server's latest table snapshot.
func (m *Monitor) Tables(label string) (DBTables, bool, error) {
	var t DBTables
	ok, err := m.Store.Blob("tables:"+label, &t)
	return t, ok, err
}
