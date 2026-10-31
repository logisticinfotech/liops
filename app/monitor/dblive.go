// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"strings"
	"sync"
	"time"
)

// DBStatus is what the monitor knows about one database server.
type DBStatus struct {
	Label   string    `json:"label"`
	Title   string    `json:"title"`
	Engine  string    `json:"engine"`
	Version string    `json:"version,omitempty"`
	Error   string    `json:"error,omitempty"` // it can't log in or read the statistics
	Checked time.Time `json:"checked"`
	// Statements is false when PostgreSQL has no pg_stat_statements (it needs a restart)
	// or MySQL's digests are off; StatementsError says why.
	Statements      bool   `json:"statements"`
	StatementsError string `json:"statements_error,omitempty"`
}

// The live numbers' SQL: name/value rows. Counters (cumulative) become rates.
const (
	pgLiveSQL = `SELECT 'connections', count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'
UNION ALL SELECT 'running', count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND state = 'active' AND pid <> pg_backend_pid()
UNION ALL SELECT 'slow', count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND state = 'active' AND pid <> pg_backend_pid() AND query_start < now() - interval '1 second'
UNION ALL SELECT 'lock_waits', count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'
UNION ALL SELECT 'long_transactions', count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND xact_start < now() - interval '1 minute'
UNION ALL SELECT 'idle_in_transaction', count(*) FROM pg_stat_activity WHERE state LIKE 'idle in transaction%' AND state_change < now() - interval '1 minute'
UNION ALL SELECT 'max_connections', setting::bigint FROM pg_settings WHERE name = 'max_connections'
UNION ALL SELECT 'c:hits', sum(blks_hit) FROM pg_stat_database
UNION ALL SELECT 'c:reads', sum(blks_read) FROM pg_stat_database
UNION ALL SELECT 'c:transactions', sum(xact_commit + xact_rollback) FROM pg_stat_database
UNION ALL SELECT 'c:deadlocks', sum(deadlocks) FROM pg_stat_database
UNION ALL SELECT 'c:temp_bytes', sum(temp_bytes) FROM pg_stat_database`

	mysqlLiveSQL = `SELECT 'connections', VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Threads_connected'
UNION ALL SELECT 'running', VARIABLE_VALUE - 1 FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Threads_running'
UNION ALL SELECT 'slow', COUNT(*) FROM performance_schema.processlist WHERE COMMAND = 'Query' AND TIME >= 1 AND ID <> CONNECTION_ID()
UNION ALL SELECT 'lock_waits', VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Innodb_row_lock_current_waits'
UNION ALL SELECT 'long_transactions', COUNT(*) FROM information_schema.INNODB_TRX WHERE trx_started < NOW() - INTERVAL 1 MINUTE
UNION ALL SELECT 'max_connections', @@max_connections
UNION ALL SELECT 'c:hits', VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Innodb_buffer_pool_read_requests'
UNION ALL SELECT 'c:reads', VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Innodb_buffer_pool_reads'
UNION ALL SELECT 'c:transactions', VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Questions'
UNION ALL SELECT 'c:deadlocks', COUNT FROM information_schema.INNODB_METRICS WHERE NAME = 'lock_deadlocks'
UNION ALL SELECT 'c:temp_tables', VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Created_tmp_disk_tables'`
)

// dbState is the monitor's per-server memory between rounds.
type dbState struct {
	mu       sync.Mutex
	status   map[string]DBStatus
	counters map[string]map[string]float64 // label → c:… → value, from the last round
	countAt  map[string]time.Time
	queries  map[string]*digestBaseline // label → last statement snapshot
}

func (m *Monitor) dbs() *dbState {
	m.dbOnce.Do(func() {
		m.dbst = &dbState{status: map[string]DBStatus{}, counters: map[string]map[string]float64{},
			countAt: map[string]time.Time{}, queries: map[string]*digestBaseline{}}
	})
	return m.dbst
}

// DBStatuses lists the database servers, in the order they were found.
func (m *Monitor) DBStatuses() []DBStatus {
	servers := m.servers()
	st := m.dbs()
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []DBStatus
	for _, d := range servers {
		if s, ok := st.status[d.Label()]; ok {
			out = append(out, s)
		} else {
			out = append(out, DBStatus{Label: d.Label(), Title: d.Title(), Engine: d.Engine})
		}
	}
	return out
}

// liveDBs reads every server's live numbers and holds them as series for the next
// samples: db.<name>:<label>, e.g. db.slow:mysql.
func (m *Monitor) liveDBs(ctx context.Context) {
	for _, d := range m.servers() {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		values, err := m.liveDB(ctx, d)
		cancel()
		m.setStatus(d, func(s *DBStatus) {
			s.Error = ""
			if err != nil {
				s.Error = err.Error()
			}
		})
		if err == nil {
			m.hold(values, 3*Interval)
		}
	}
}

func (m *Monitor) liveDB(ctx context.Context, d DBServer) (map[string]float64, error) {
	sql := pgLiveSQL
	if d.Engine == EngineMySQL {
		sql = mysqlLiveSQL
	}
	rows, err := d.Query(ctx, "", sql)
	if err != nil {
		return nil, err
	}
	now := m.Now()
	label := d.Label()
	values := map[string]float64{}
	counters := map[string]float64{}
	for _, r := range rows {
		if len(r) != 2 {
			continue
		}
		if name, ok := strings.CutPrefix(r[0], "c:"); ok {
			counters[name] = atof(r[1])
			continue
		}
		values["db."+r[0]+":"+label] = atof(r[1])
	}

	st := m.dbs()
	st.mu.Lock()
	prev, at := st.counters[label], st.countAt[label]
	st.counters[label], st.countAt[label] = counters, now
	st.mu.Unlock()
	dt := now.Sub(at).Seconds()
	if prev == nil || dt <= 0 {
		return values, nil
	}
	delta := func(name string) (float64, bool) {
		d := counters[name] - prev[name]
		return d, d >= 0 // a lower counter: the statistics were reset
	}
	hits, ok1 := delta("hits")
	reads, ok2 := delta("reads")
	if ok1 && ok2 && hits+reads > 0 {
		// MySQL counts every read request in hits; PostgreSQL counts hits and reads apart.
		total := hits + reads
		if d.Engine == EngineMySQL {
			total = hits
		}
		if total > 0 {
			values["db.cache_hit:"+label] = 100 * (1 - reads/total)
		}
	}
	if n, ok := delta("transactions"); ok {
		values["db.per_second:"+label] = n / dt
	}
	if n, ok := delta("deadlocks"); ok {
		values["db.deadlocks:"+label] = n
	}
	if n, ok := delta("temp_tables"); ok {
		values["db.temp_tables:"+label] = n / dt
	}
	if n, ok := delta("temp_bytes"); ok {
		values["db.temp_bytes:"+label] = n / dt
	}
	return values, nil
}

func (m *Monitor) setStatus(d DBServer, change func(*DBStatus)) {
	st := m.dbs()
	st.mu.Lock()
	defer st.mu.Unlock()
	s, ok := st.status[d.Label()]
	if !ok {
		s = DBStatus{Label: d.Label(), Title: d.Title(), Engine: d.Engine}
	}
	s.Checked = m.Now()
	change(&s)
	st.status[d.Label()] = s
}
