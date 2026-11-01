// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Statement statistics: numbers for every shape each round (no text, to keep it small),
// then the text of shapes the store hasn't seen. The monitor's own statements aren't
// counted: on MySQL they run without a default database, on PostgreSQL as its own role.
const (
	mysqlDigestSQL = `SELECT HEX(SCHEMA_NAME), DIGEST, COUNT_STAR, SUM_TIMER_WAIT / 1000000000, MAX_TIMER_WAIT / 1000000000,
SUM_ROWS_EXAMINED, SUM_ROWS_SENT, SUM_NO_INDEX_USED + SUM_NO_GOOD_INDEX_USED, SUM_CREATED_TMP_DISK_TABLES, UNIX_TIMESTAMP(FIRST_SEEN)
FROM performance_schema.events_statements_summary_by_digest
WHERE DIGEST IS NOT NULL AND SCHEMA_NAME IS NOT NULL AND SCHEMA_NAME NOT IN ('mysql', 'sys', 'performance_schema', 'information_schema')`

	mysqlDigestTextSQL = `SELECT HEX(SCHEMA_NAME), DIGEST, HEX(DIGEST_TEXT)
FROM performance_schema.events_statements_summary_by_digest WHERE DIGEST IN (%s)`

	pgDigestSQL = `SELECT encode(convert_to(d.datname, 'UTF8'), 'hex'), s.queryid, s.calls, s.total_exec_time, s.max_exec_time,
s.shared_blks_hit + s.shared_blks_read, s.rows, 0, s.temp_blks_written, 0
FROM pg_stat_statements s JOIN pg_database d ON d.oid = s.dbid
WHERE s.queryid IS NOT NULL AND s.userid <> (SELECT oid FROM pg_roles WHERE rolname = current_user)`

	pgDigestTextSQL = `SELECT DISTINCT ON (s.dbid, s.queryid) encode(convert_to(d.datname, 'UTF8'), 'hex'), s.queryid,
encode(convert_to(left(s.query, 4000), 'UTF8'), 'hex')
FROM pg_stat_statements s JOIN pg_database d ON d.oid = s.dbid WHERE s.queryid IN (%s)`
)

// digestRow is one shape's cumulative numbers, as the database reports them.
type digestRow struct {
	shape QueryShape
	c     QueryCounters
}

// digestBaseline is the last round's numbers, to subtract from the next.
type digestBaseline struct {
	at   time.Time
	rows map[string]digestRow
}

var (
	mysqlDigestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	pgQueryIDRE   = regexp.MustCompile(`^-?[0-9]{1,20}$`)
)

// readDigests reads every shape's cumulative numbers.
func readDigests(ctx context.Context, d DBServer) (map[string]digestRow, error) {
	sql, idRE := pgDigestSQL, pgQueryIDRE
	if d.Engine == EngineMySQL {
		sql, idRE = mysqlDigestSQL, mysqlDigestRE
	}
	rows, err := d.Query(ctx, "", sql)
	if err != nil {
		return nil, err
	}
	out := map[string]digestRow{}
	for _, r := range rows {
		if len(r) != 10 || !idRE.MatchString(r[1]) {
			continue
		}
		q := QueryShape{Server: d.Label(), DB: unhex(r[0]), ID: r[1], MaxMS: atof(r[4])}
		if first := atof(r[9]); first > 0 {
			q.FirstSeen = time.Unix(int64(first), 0)
		}
		c := QueryCounters{Calls: atof(r[2]), TimeMS: atof(r[3]), RowsExamined: atof(r[5]), RowsSent: atof(r[6]),
			NoIndex: atof(r[7]), TempDisk: atof(r[8])}
		// PostgreSQL has a row per role (and per top-level flag): add them up.
		if prev, ok := out[q.key()]; ok {
			c.add(prev.c)
			q.MaxMS = math.Max(q.MaxMS, prev.shape.MaxMS)
		}
		out[q.key()] = digestRow{q, c}
	}
	return out, nil
}

// diffDigests is what happened between two rounds: each shape's numbers minus the last
// round's. A shape that's new, or whose calls went down (the statistics were reset),
// counts from zero.
func diffDigests(prev, cur map[string]digestRow) []QueryTotal {
	var out []QueryTotal
	for k, c := range cur {
		d := c.c
		if p, ok := prev[k]; ok && c.c.Calls >= p.c.Calls {
			d = QueryCounters{Calls: c.c.Calls - p.c.Calls, TimeMS: c.c.TimeMS - p.c.TimeMS,
				RowsExamined: c.c.RowsExamined - p.c.RowsExamined, RowsSent: c.c.RowsSent - p.c.RowsSent,
				NoIndex: c.c.NoIndex - p.c.NoIndex, TempDisk: c.c.TempDisk - p.c.TempDisk}
		}
		if d.Calls > 0 {
			out = append(out, QueryTotal{QueryShape: c.shape, QueryCounters: d})
		}
	}
	return out
}

// readDigestTexts fills in the text of the given shapes, asking 200 at a time.
func readDigestTexts(ctx context.Context, d DBServer, rows []QueryTotal) error {
	sql := pgDigestTextSQL
	if d.Engine == EngineMySQL {
		sql = mysqlDigestTextSQL
	}
	byKey := map[string]*QueryTotal{}
	var ids []string
	seen := map[string]bool{}
	for i := range rows {
		byKey[rows[i].key()] = &rows[i]
		if !seen[rows[i].ID] {
			seen[rows[i].ID] = true
			if d.Engine == EngineMySQL {
				ids = append(ids, "'"+rows[i].ID+"'") // checked against mysqlDigestRE
			} else {
				ids = append(ids, rows[i].ID)
			}
		}
	}
	for len(ids) > 0 {
		batch := ids[:min(200, len(ids))]
		ids = ids[len(batch):]
		texts, err := d.Query(ctx, "", fmt.Sprintf(sql, strings.Join(batch, ", ")))
		if err != nil {
			return err
		}
		for _, t := range texts {
			if len(t) != 3 {
				continue
			}
			k := QueryShape{Server: d.Label(), DB: unhex(t[0]), ID: t[1]}.key()
			if r, ok := byKey[k]; ok {
				r.Text = unhex(t[2])
			}
		}
	}
	return nil
}

// snapshotQueries reads a server's statement statistics and stores what changed since
// the last round. The first round after the monitor starts only sets the baseline.
func (m *Monitor) snapshotQueries(ctx context.Context, d DBServer) error {
	cur, err := readDigests(ctx, d)
	m.setStatus(d, func(s *DBStatus) {
		s.Statements, s.StatementsError = err == nil, ""
		if err != nil {
			s.StatementsError = statementsProblem(d, err)
		}
	})
	if err != nil {
		return err
	}
	st := m.dbs()
	st.mu.Lock()
	prev := st.queries[d.Label()]
	st.queries[d.Label()] = &digestBaseline{at: m.Now(), rows: cur}
	st.mu.Unlock()
	if prev == nil {
		return nil
	}
	rows := diffDigests(prev.rows, cur)
	if len(rows) == 0 {
		return nil
	}
	// Only the busiest are stored; fetch the text of those the store doesn't know.
	rows = topByTime(rows, maxQueryRows)
	known := m.Store.KnownShapes(rows)
	var need []QueryTotal
	for _, r := range rows {
		if !known[r.key()] {
			need = append(need, r)
		}
	}
	if len(need) > 0 {
		if err := readDigestTexts(ctx, d, need); err != nil {
			return err
		}
		texts := map[string]string{}
		for _, r := range need {
			texts[r.key()] = r.Text
		}
		for i := range rows {
			if t, ok := texts[rows[i].key()]; ok {
				rows[i].Text = t
			}
		}
	}
	// The snapshot covers the 5 minutes before the mark it's taken at: record it in those
	// 5 minutes (a second before the mark), like the request records.
	at := m.Now()
	if mark := at.Truncate(QueryInterval); at.Sub(mark) < time.Minute {
		at = mark.Add(-time.Second)
	}
	return m.Store.AddQueries(at, rows)
}

func topByTime(rows []QueryTotal, n int) []QueryTotal {
	if len(rows) <= n {
		return rows
	}
	sortByTime(rows)
	return rows[:n]
}

// statementsProblem says in plain words why the statement statistics can't be read.
func statementsProblem(d DBServer, err error) string {
	msg := err.Error()
	if d.Engine == EnginePostgres {
		switch {
		case strings.Contains(msg, "shared_preload_libraries"):
			return "pg_stat_statements is installed but not loaded yet: PostgreSQL needs the restart shown in Settings → Monitoring."
		case strings.Contains(msg, "pg_stat_statements") && strings.Contains(msg, "does not exist"):
			return "pg_stat_statements isn't set up: use Set up database access in Settings → Monitoring."
		}
	}
	return msg
}
