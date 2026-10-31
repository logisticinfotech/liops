// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// servers returns the database servers found last.
func (m *Monitor) servers() []DBServer {
	st := m.dbs()
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]DBServer(nil), m.DBs...)
}

// dbLoop reads the databases: live numbers every Interval, statement statistics every
// QueryInterval, tables hourly. A slow or stuck database never delays the machine's
// samples, which run apart.
func (m *Monitor) dbLoop(ctx context.Context) {
	rediscover := func() {
		if m.FindDBs == nil {
			return
		}
		found := m.FindDBs()
		st := m.dbs()
		st.mu.Lock()
		m.DBs = found
		st.mu.Unlock()
	}
	queries := func() {
		for _, d := range m.servers() {
			ctx, cancel := context.WithTimeout(ctx, time.Minute)
			if err := m.snapshotQueries(ctx, d); err != nil {
				m.logf("%s statements: %v", d.Label(), err)
			}
			cancel()
		}
	}
	tables := func() {
		for _, d := range m.servers() {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			err := m.readTables(ctx, d)
			cancel()
			if err != nil {
				m.logf("%s tables: %v", d.Label(), err)
			}
		}
	}
	rediscover()
	m.liveDBs(ctx)
	queries()
	tables()
	live := time.NewTicker(Interval)
	defer live.Stop()
	stmts := time.NewTicker(QueryInterval)
	defer stmts.Stop()
	hourly := time.NewTicker(TableInterval)
	defer hourly.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-live.C:
			m.liveDBs(ctx)
		case <-stmts.C:
			rediscover()
			queries()
		case <-hourly.C:
			tables()
		}
	}
}

// RunningStatement is a statement running now. Query holds real values: it's read when
// a person opens the page, and never stored or logged.
type RunningStatement struct {
	PID       int64   `json:"pid"`
	DB        string  `json:"db"`
	User      string  `json:"user"`
	State     string  `json:"state"`
	Seconds   float64 `json:"seconds"`
	TxSeconds float64 `json:"tx_seconds,omitempty"` // PostgreSQL: the transaction's age
	Waiting   string  `json:"waiting,omitempty"`    // what it waits for (PostgreSQL's wait_event_type)
	BlockedBy string  `json:"blocked_by,omitempty"` // PIDs holding the lock it waits for
	Query     string  `json:"query"`
}

// OpenTransaction is a transaction open for a while (MySQL; PostgreSQL's are in the
// statements, with TxSeconds).
type OpenTransaction struct {
	PID          int64   `json:"pid"`
	Seconds      float64 `json:"seconds"`
	State        string  `json:"state"`
	RowsModified float64 `json:"rows_modified"`
}

// Activity is what a server is doing right now.
type Activity struct {
	Statements   []RunningStatement `json:"statements"`
	Transactions []OpenTransaction  `json:"transactions,omitempty"`
}

func readActivity(ctx context.Context, d DBServer) (Activity, error) {
	var a Activity
	if d.Engine == EnginePostgres {
		rows, err := d.Query(ctx, "", `SELECT pid, encode(convert_to(coalesce(datname, ''), 'UTF8'), 'hex'), encode(convert_to(coalesce(usename, ''), 'UTF8'), 'hex'),
coalesce(state, ''), coalesce(extract(epoch FROM now() - query_start), 0), coalesce(extract(epoch FROM now() - xact_start), 0),
coalesce(wait_event_type, ''), array_to_string(pg_blocking_pids(pid), ' '), encode(convert_to(left(query, 2000), 'UTF8'), 'hex')
FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid() AND state <> 'idle'
ORDER BY query_start NULLS LAST LIMIT 100`)
		if err != nil {
			return a, err
		}
		for _, r := range rows {
			if len(r) == 9 {
				pid, _ := strconv.ParseInt(r[0], 10, 64)
				a.Statements = append(a.Statements, RunningStatement{PID: pid, DB: unhex(r[1]), User: unhex(r[2]), State: r[3],
					Seconds: atof(r[4]), TxSeconds: atof(r[5]), Waiting: r[6], BlockedBy: r[7], Query: unhex(r[8])})
			}
		}
		return a, nil
	}
	rows, err := d.Query(ctx, "", `SELECT p.ID, HEX(IFNULL(p.DB, '')), HEX(IFNULL(p.USER, '')), IFNULL(p.STATE, ''), p.TIME,
IFNULL((SELECT GROUP_CONCAT(w.blocking_pid SEPARATOR ' ') FROM sys.innodb_lock_waits w WHERE w.waiting_pid = p.ID), ''),
HEX(LEFT(IFNULL(p.INFO, ''), 2000))
FROM performance_schema.processlist p WHERE p.COMMAND NOT IN ('Sleep', 'Daemon', 'Binlog Dump', 'Binlog Dump GTID') AND p.ID <> CONNECTION_ID()
ORDER BY p.TIME DESC LIMIT 100`)
	if err != nil {
		return a, err
	}
	for _, r := range rows {
		if len(r) == 7 {
			pid, _ := strconv.ParseInt(r[0], 10, 64)
			s := RunningStatement{PID: pid, DB: unhex(r[1]), User: unhex(r[2]), State: r[3], Seconds: atof(r[4]), BlockedBy: r[5], Query: unhex(r[6])}
			if s.BlockedBy != "" {
				s.Waiting = "Lock"
			}
			a.Statements = append(a.Statements, s)
		}
	}
	rows, err = d.Query(ctx, "", `SELECT trx_mysql_thread_id, TIMESTAMPDIFF(SECOND, trx_started, NOW()), trx_state, trx_rows_modified
FROM information_schema.INNODB_TRX WHERE trx_started < NOW() - INTERVAL 10 SECOND ORDER BY trx_started LIMIT 50`)
	if err != nil {
		return a, err
	}
	for _, r := range rows {
		if len(r) == 4 {
			pid, _ := strconv.ParseInt(r[0], 10, 64)
			a.Transactions = append(a.Transactions, OpenTransaction{PID: pid, Seconds: atof(r[1]), State: r[2], RowsModified: atof(r[3])})
		}
	}
	return a, nil
}

// QuerySample is one real statement of a query shape (MySQL keeps the slowest recent one).
// It holds real values: read live when a person asks, never stored or logged.
type QuerySample struct {
	Text string    `json:"text"`
	Seen time.Time `json:"seen"`
	MS   float64   `json:"ms"`
}

func readSample(ctx context.Context, d DBServer, db, digest string) (QuerySample, bool, error) {
	if d.Engine != EngineMySQL || !mysqlDigestRE.MatchString(digest) {
		return QuerySample{}, false, nil
	}
	rows, err := d.Query(ctx, "", `SELECT HEX(IFNULL(QUERY_SAMPLE_TEXT, '')), IFNULL(UNIX_TIMESTAMP(QUERY_SAMPLE_SEEN), 0), QUERY_SAMPLE_TIMER_WAIT / 1000000000
FROM performance_schema.events_statements_summary_by_digest WHERE SCHEMA_NAME = UNHEX('`+hex.EncodeToString([]byte(db))+`') AND DIGEST = '`+digest+`'`)
	if err != nil || len(rows) == 0 || len(rows[0]) != 3 || rows[0][0] == "" {
		return QuerySample{}, false, err
	}
	return QuerySample{Text: unhex(rows[0][0]), Seen: time.Unix(int64(atof(rows[0][1])), 0), MS: atof(rows[0][2])}, true, nil
}

// QueryDetailResponse is one query shape and its history.
type QueryDetailResponse struct {
	Shape  QueryShape   `json:"shape"`
	Points []QueryPoint `json:"points"`
}

// dbHandlers adds the database API to mux.
func (m *Monitor) dbHandlers(mux *http.ServeMux, writeJSON func(http.ResponseWriter, any)) {
	server := func(w http.ResponseWriter, r *http.Request) (DBServer, bool) {
		for _, d := range m.servers() {
			if d.Label() == r.URL.Query().Get("server") {
				return d, true
			}
		}
		http.Error(w, "no such database server", http.StatusNotFound)
		return DBServer{}, false
	}
	span := func(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, bool) {
		d, ok := Ranges[r.URL.Query().Get("range")]
		if !ok {
			http.Error(w, "range must be 1h, 24h, 7d or 90d", http.StatusBadRequest)
		}
		now := m.Now()
		return now.Add(-d), now, ok
	}
	live := func(r *http.Request) (context.Context, context.CancelFunc) {
		return context.WithTimeout(r.Context(), 15*time.Second)
	}
	fail := func(w http.ResponseWriter, err error) {
		status := http.StatusBadGateway // the database didn't answer
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, err.Error(), status)
	}

	mux.HandleFunc("GET /v1/db", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, m.DBStatuses()) })
	mux.HandleFunc("GET /v1/db/queries", func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := span(w, r)
		if !ok {
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		top, err := m.Store.TopQueries(from, to, r.URL.Query().Get("server"), r.URL.Query().Get("db"), min(max(limit, 1), 500))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, top)
	})
	mux.HandleFunc("GET /v1/db/query", func(w http.ResponseWriter, r *http.Request) {
		from, to, ok := span(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		shape, points, found, err := m.Store.QueryDetail(q.Get("server"), q.Get("db"), q.Get("id"), from, to)
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !found:
			http.Error(w, "no such query", http.StatusNotFound)
		default:
			writeJSON(w, QueryDetailResponse{Shape: shape, Points: points})
		}
	})
	mux.HandleFunc("GET /v1/db/sample", func(w http.ResponseWriter, r *http.Request) {
		d, ok := server(w, r)
		if !ok {
			return
		}
		ctx, cancel := live(r)
		defer cancel()
		s, found, err := readSample(ctx, d, r.URL.Query().Get("db"), r.URL.Query().Get("id"))
		switch {
		case err != nil:
			fail(w, err)
		case !found:
			http.Error(w, "no sample", http.StatusNotFound)
		default:
			writeJSON(w, s)
		}
	})
	mux.HandleFunc("GET /v1/db/activity", func(w http.ResponseWriter, r *http.Request) {
		d, ok := server(w, r)
		if !ok {
			return
		}
		ctx, cancel := live(r)
		defer cancel()
		a, err := readActivity(ctx, d)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, a)
	})
	mux.HandleFunc("GET /v1/db/tables", func(w http.ResponseWriter, r *http.Request) {
		t, found, err := m.Tables(r.URL.Query().Get("server"))
		switch {
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		case !found:
			http.Error(w, "not read yet", http.StatusNotFound)
		default:
			writeJSON(w, t)
		}
	})
}
