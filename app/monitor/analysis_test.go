// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"slices"
	"strings"
	"testing"
)

func TestQueryFlags(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string // flag names start with these
	}{
		// MySQL digests, as performance_schema prints them.
		{"SELECT `id` FROM `readings` WHERE DATE ( `created_at` ) = ?", []string{"DATE(created_at)"}},
		{"SELECT * FROM `customers` WHERE LOWER ( `name` ) LIKE ? OR LOWER ( `email` ) LIKE ?", []string{"LOWER(name)"}},
		{"SELECT `id` FROM `users` WHERE `email` = ? OR `phone` = ?", []string{"OR across columns (email, phone)"}},
		{"SELECT `id` FROM `users` WHERE `status` = ? OR `status` = ?", nil},
		{"SELECT `id` FROM `users` WHERE `email` = ?", nil},
		{"SELECT DATE ( `created_at` ) FROM `readings`", nil}, // in the select list: fine
		// PostgreSQL.
		{"SELECT id FROM readings WHERE date(created_at) = $1", []string{"DATE(created_at)"}},
		{"SELECT id FROM t WHERE lower(t.name) LIKE $1", []string{"LOWER(t.name)"}},
		{"SELECT id FROM t WHERE created_at::date = $1", nil},
		{"SELECT id FROM t WHERE lower($1) = name", nil}, // a function on the value: fine
	} {
		var names []string
		for _, f := range QueryFlags(c.text, QueryCounters{Calls: 1, RowsSent: 1}) {
			names = append(names, f.Name)
		}
		if len(names) != len(c.want) {
			t.Errorf("%s: flags %q, want %q", c.text, names, c.want)
			continue
		}
		for i := range names {
			if !strings.HasPrefix(names[i], c.want[i]) {
				t.Errorf("%s: flag %q, want %q…", c.text, names[i], c.want[i])
			}
		}
	}
	if f := QueryFlags("SELECT * FROM `logs` WHERE `a` = ?", QueryCounters{Calls: 2, RowsSent: 5000}); len(f) != 1 || !strings.HasPrefix(f[0].Name, "SELECT *") {
		t.Errorf("SELECT * with many rows: %v", f)
	}
	if f := SampleFlags("SELECT * FROM customers WHERE LOWER(name) LIKE '%acme%'"); len(f) != 1 {
		t.Errorf("leading wildcard: %v", f)
	}
	if f := SampleFlags("SELECT * FROM customers WHERE name LIKE 'acme%'"); f != nil {
		t.Errorf("a trailing wildcard can use an index: %v", f)
	}
}

func TestQueryEfficiency(t *testing.T) {
	// §1.1: 1.65 million rows examined to return 1.
	e := QueryEfficiency(EngineMySQL, QueryCounters{Calls: 2, RowsExamined: 3.3e6, RowsSent: 2})
	if !e.Scans || e.Ratio != 1.65e6 || e.PerCall != 1.65e6 {
		t.Errorf("search: %+v", e)
	}
	if e := QueryEfficiency(EngineMySQL, QueryCounters{Calls: 1000, RowsExamined: 1000, RowsSent: 1000}); e.Scans {
		t.Errorf("a lookup by key: %+v", e)
	}
	if e := QueryEfficiency(EngineMySQL, QueryCounters{Calls: 10, RowsExamined: 20000, RowsSent: 10, NoIndex: 10}); !e.Scans {
		t.Errorf("no index on a 2,000-row table: %+v", e)
	}
	if e := QueryEfficiency(EnginePostgres, QueryCounters{Calls: 3, RowsExamined: 3 * 8000, RowsSent: 3}); !e.Scans {
		t.Errorf("postgres scan: %+v", e)
	}
	if e := QueryEfficiency(EnginePostgres, QueryCounters{Calls: 3, RowsExamined: 3 * 8000, RowsSent: 3 * 5000}); e.Scans {
		t.Errorf("a report that returns what it reads: %+v", e)
	}
}

func TestTablesIn(t *testing.T) {
	got := TablesIn("SELECT * FROM `shop` . `orders` JOIN `customers` ON ... WHERE `id` IN ( SELECT `x` FROM `orders` )")
	if !slices.Equal(got, []string{"shop.orders", "customers", "orders"}) {
		t.Errorf("got %q", got)
	}
	if got := TablesIn(`UPDATE public.readings SET v = $1`); !slices.Equal(got, []string{"public.readings"}) {
		t.Errorf("got %q", got)
	}
}
