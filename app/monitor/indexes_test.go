// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"strings"
	"testing"
)

func TestSuggestIndex(t *testing.T) {
	pk := []IndexInfo{{Table: "readings", Name: "PRIMARY", Definition: "(id)"}}
	s := SuggestIndex("SELECT `id` , `v` FROM `readings` WHERE `tenant_id` = ? AND DATE ( `created_at` ) = ? ORDER BY `id`", "readings", pk)
	if s == nil || strings.Join(s.Columns, ",") != "tenant_id,created_at" || !strings.Contains(s.Note, "range") ||
		s.SQL != "CREATE INDEX idx_readings_tenant_id_created_at ON readings (tenant_id, created_at);" ||
		!strings.Contains(s.Migration, "$table->index(['tenant_id', 'created_at']);") {
		t.Errorf("s = %+v", s)
	}
	// PostgreSQL's definitions, and an index that already fits.
	pg := []IndexInfo{{Table: "orders", Name: "orders_customer_id_idx", Definition: "CREATE INDEX orders_customer_id_idx ON public.orders USING btree (customer_id, created_at)"}}
	if s := SuggestIndex("SELECT * FROM orders WHERE customer_id = $1 AND created_at > $2", "orders", pg); s != nil {
		t.Errorf("covered already: %+v", s)
	}
	if s := SuggestIndex("SELECT * FROM orders WHERE status IN ($1, $2)", "orders", pg); s == nil || s.Columns[0] != "status" {
		t.Errorf("status: %+v", s)
	}
	// LOWER(col) LIKE '%x%': no index helps; nothing to suggest.
	if s := SuggestIndex("SELECT * FROM `customers` WHERE `LOWER` ( NAME ) LIKE ?", "customers", pk); s != nil {
		t.Errorf("a search inside text: %+v", s)
	}
}
