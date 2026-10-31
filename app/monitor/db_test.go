// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func hx(s string) string { return hex.EncodeToString([]byte(s)) }

// fakeMySQL answers the monitor's statements like MySQL would, from counters the test sets.
type fakeMySQL struct {
	mu        sync.Mutex
	calls     float64 // the search shape's COUNT_STAR
	reset     bool
	textReads int
	args      [][]string
}

const searchDigest = "1111111111111111111111111111111111111111111111111111111111111111"

func (f *fakeMySQL) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.args = append(f.args, args)
	if name != "mysql" || args[0] != "--no-defaults" {
		return nil, fmt.Errorf("unexpected %s %v", name, args)
	}
	sql := args[len(args)-1]
	switch {
	case strings.Contains(sql, "Threads_connected"):
		return []byte(fmt.Sprintf("connections\t12\nrunning\t2\nslow\t1\nlock_waits\t0\nlong_transactions\t0\nmax_connections\t151\nc:hits\t%d\nc:reads\t%d\nc:transactions\t%d\nc:deadlocks\t0\nc:temp_tables\t0\n",
			int(1000+f.calls*1000), int(10+f.calls*10), int(f.calls*100))), nil
	case strings.Contains(sql, "QUERY_SAMPLE_TEXT"):
		return []byte(hx("SELECT * FROM customers WHERE LOWER(name) LIKE '%acme%'") + "\t1700000000\t2650.5\n"), nil
	case strings.Contains(sql, "HEX(DIGEST_TEXT)"):
		f.textReads++
		return []byte(hx("tenant_a") + "\t" + searchDigest + "\t" + hx("SELECT * FROM `customers` WHERE LOWER ( `name` ) LIKE ?") + "\n"), nil
	case strings.Contains(sql, "events_statements_summary_by_digest"):
		// 1.65 million rows examined per call, one row sent.
		c := f.calls
		return []byte(fmt.Sprintf("%s\t%s\t%g\t%g\t2900\t%g\t%g\t0\t0\t1700000000\n", hx("tenant_a"), searchDigest, c, c*2500, c*1.65e6, c)), nil
	case strings.Contains(sql, "information_schema.TABLES"):
		return []byte(hx("tenant_a") + "\t" + hx("customers") + "\t1650000\t" + fmt.Sprint(int(400e6+f.calls*1e6)) + "\n"), nil
	case strings.Contains(sql, "information_schema.STATISTICS"):
		return []byte(hx("tenant_a") + "\t" + hx("customers") + "\t" + hx("PRIMARY") + "\t0\t" + hx("id") + "\n" +
			hx("tenant_a") + "\t" + hx("customers") + "\t" + hx("tenant_email") + "\t1\t" + hx("tenant_id") + "\n" +
			hx("tenant_a") + "\t" + hx("customers") + "\t" + hx("tenant_email") + "\t1\t" + hx("email") + "\n" +
			hx("tenant_a") + "\t" + hx("customers") + "\t" + hx("tenant_only") + "\t1\t" + hx("tenant_id") + "\n"), nil
	case strings.Contains(sql, "schema_unused_indexes"):
		return []byte(hx("tenant_a") + "\t" + hx("customers") + "\t" + hx("tenant_only") + "\n"), nil
	case strings.Contains(sql, "schema_redundant_indexes"):
		return []byte(hx("tenant_a") + "\t" + hx("customers") + "\t" + hx("tenant_only") + "\t" + hx("tenant_email") + "\n"), nil
	case strings.Contains(sql, "innodb_buffer_pool_size"):
		return []byte("innodb_buffer_pool_size\t134217728\nuptime\t86400\n"), nil
	case strings.Contains(sql, "performance_schema.processlist p"):
		return []byte("42\t" + hx("tenant_a") + "\t" + hx("app") + "\texecuting\t3\t\t" + hx("SELECT 1") + "\n"), nil
	case strings.Contains(sql, "INNODB_TRX"):
		return []byte("43\t95\tRUNNING\t1200\n"), nil
	}
	return nil, fmt.Errorf("unexpected SQL %q", sql)
}

func newDBMonitor(t *testing.T, f *fakeMySQL) (*Monitor, *time.Time) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	m := New(NewCollector(newFakeHost(t).root), store, nil)
	clock := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	m.Now = func() time.Time { return clock }
	m.DBs = []DBServer{{Engine: EngineMySQL, Socket: "/run/mysqld/mysqld.sock", Exec: f.exec}}
	return m, &clock
}

func TestDatabaseStatements(t *testing.T) {
	f := &fakeMySQL{calls: 10}
	m, clock := newDBMonitor(t, f)
	ctx := context.Background()
	d := m.DBs[0]

	// The first round sets the baseline only.
	if err := m.snapshotQueries(ctx, d); err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 3; round++ {
		f.calls += 2
		*clock = clock.Add(QueryInterval)
		if err := m.snapshotQueries(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if f.textReads != 1 {
		t.Errorf("the text should be read once, then kept: %d reads", f.textReads)
	}
	top, err := m.Store.TopQueries(clock.Add(-time.Hour), *clock, "", "", 10)
	if err != nil || len(top) != 1 {
		t.Fatalf("top = %+v, %v", top, err)
	}
	q := top[0]
	if q.Calls != 6 || q.TimeMS != 15000 || q.RowsExamined != 6*1.65e6 || q.RowsSent != 6 || q.DB != "tenant_a" || !strings.Contains(q.Text, "LOWER") || q.MaxMS != 2900 {
		t.Errorf("q = %+v", q)
	}
	if e := QueryEfficiency(EngineMySQL, q.QueryCounters); !e.Scans || e.Ratio != 1.65e6 {
		t.Errorf("efficiency = %+v", e)
	}

	// Reset statistics: the new, lower counts are what happened since.
	f.calls = 1
	*clock = clock.Add(QueryInterval)
	if err := m.snapshotQueries(ctx, d); err != nil {
		t.Fatal(err)
	}
	_, points, ok, err := m.Store.QueryDetail("mysql", "tenant_a", searchDigest, clock.Add(-time.Hour), *clock)
	if err != nil || !ok || len(points) != 4 || points[3].Calls != 1 {
		t.Errorf("points = %+v, %v %v", points, ok, err)
	}
}

func TestDatabaseLiveAndTables(t *testing.T) {
	f := &fakeMySQL{calls: 10}
	m, clock := newDBMonitor(t, f)
	ctx := context.Background()
	m.liveDBs(ctx)
	f.calls += 15
	*clock = clock.Add(Interval)
	m.liveDBs(ctx)
	if err := m.collect(); err != nil {
		t.Fatal(err)
	}
	s, _ := m.Store.Latest()
	if s.Values["db.connections:mysql"] != 12 || s.Values["db.slow:mysql"] != 1 || s.Values["db.per_second:mysql"] != 100 ||
		s.Values["db.cache_hit:mysql"] != 99 {
		t.Errorf("values = %v", s.Values)
	}
	if st := m.DBStatuses(); len(st) != 1 || st[0].Error != "" || st[0].Title != "MySQL" {
		t.Errorf("status = %+v", st)
	}

	if err := m.readTables(ctx, m.DBs[0]); err != nil {
		t.Fatal(err)
	}
	f.calls += 24 // a day later the table is 24 MB bigger
	*clock = clock.Add(24 * time.Hour)
	if err := m.readTables(ctx, m.DBs[0]); err != nil {
		t.Fatal(err)
	}
	tb, ok, err := m.Tables("mysql")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if len(tb.Tables) != 1 || !tb.Tables[0].PrimaryKey || tb.Growth["tenant_a/customers"] != 24e6 || tb.Memory["innodb_buffer_pool_size"] != 128<<20 {
		t.Errorf("tables = %+v", tb)
	}
	var unused, dup *IndexInfo
	for i, ix := range tb.Indexes {
		if ix.Name == "tenant_only" {
			unused, dup = &tb.Indexes[i], &tb.Indexes[i]
		}
		if ix.Name == "tenant_email" && ix.Definition != "(tenant_id, email)" {
			t.Errorf("definition = %q", ix.Definition)
		}
	}
	if unused == nil || !unused.Unused || dup.Duplicate != "tenant_email" || len(tb.Indexes) != 3 {
		t.Errorf("indexes = %+v", tb.Indexes)
	}
}

func TestDatabaseAPI(t *testing.T) {
	f := &fakeMySQL{calls: 10}
	m, clock := newDBMonitor(t, f)
	m.Now = time.Now
	*clock = time.Now()
	ctx := context.Background()
	m.snapshotQueries(ctx, m.DBs[0])
	f.calls = 12
	m.snapshotQueries(ctx, m.DBs[0])
	m.readTables(ctx, m.DBs[0])

	socket := shortSocket(t)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go Serve(sctx, socket, m.Handler())
	c := NewClient(socket)
	var dbs []DBStatus
	var err error
	for range 50 {
		if dbs, err = c.DBs(ctx); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || len(dbs) != 1 || !dbs[0].Statements {
		t.Fatalf("dbs = %+v, %v", dbs, err)
	}
	top, err := c.Queries(ctx, "24h", "mysql", "tenant_a", 50)
	if err != nil || len(top) != 1 || top[0].Calls != 2 {
		t.Fatalf("queries = %+v, %v", top, err)
	}
	detail, err := c.Query(ctx, "24h", "mysql", "tenant_a", searchDigest)
	if err != nil || len(detail.Points) != 1 || !strings.Contains(detail.Shape.Text, "customers") {
		t.Errorf("detail = %+v, %v", detail, err)
	}
	sample, err := c.Sample(ctx, "mysql", "tenant_a", searchDigest)
	if err != nil || !strings.Contains(sample.Text, "%acme%") || len(SampleFlags(sample.Text)) != 1 {
		t.Errorf("sample = %+v, %v", sample, err)
	}
	if !strings.Contains(strings.Join(f.args[len(f.args)-1], " "), "UNHEX('"+hx("tenant_a")+"')") {
		t.Error("the database name should reach SQL hex-encoded")
	}
	act, err := c.Activity(ctx, "mysql")
	if err != nil || len(act.Statements) != 1 || act.Statements[0].PID != 42 || len(act.Transactions) != 1 || act.Transactions[0].Seconds != 95 {
		t.Errorf("activity = %+v, %v", act, err)
	}
	if tb, err := c.Tables(ctx, "mysql"); err != nil || len(tb.Tables) != 1 {
		t.Errorf("tables = %+v, %v", tb, err)
	}
	if _, err := c.Activity(ctx, "nope"); err == nil {
		t.Error("an unknown server should be refused")
	}
	if _, err := c.Sample(ctx, "mysql", "tenant_a", "'; DROP TABLE x; --"); err == nil {
		t.Error("a bad digest should be refused")
	}
}

func TestFindDBServers(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{".s.PGSQL.5432", ".s.PGSQL.5432.lock", ".s.PGSQL.5433"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o600)
	}
	sock := filepath.Join(dir, "mysqld.sock")
	os.WriteFile(sock, nil, 0o600)
	got := FindDBServers(dir, sock)
	if len(got) != 3 || got[0].Label() != "postgres" || got[1].Label() != "postgres-5433" || got[1].Title() != "PostgreSQL (port 5433)" || got[2].Label() != "mysql" {
		t.Errorf("got %+v", got)
	}
}
