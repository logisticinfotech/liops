// SPDX-License-Identifier: Apache-2.0

package dumps

import (
	"strings"
	"testing"
	"time"
)

func TestKeyRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 5, 1, 31, 2, 0, time.FixedZone("IST", 5*3600+1800))
	for _, ext := range []string{".dump", ".sql.gz"} {
		for _, db := range []string{"app", "tenant_abc", "odd/name", "with space", "100%", "dotted.name"} {
			key := Key(db, at, ext)
			if strings.Count(key, "/") != 2 {
				t.Errorf("key %q should have exactly two slashes", key)
			}
			gotDB, gotT, ok := ParseKey(key)
			if !ok || gotDB != db || !gotT.Equal(at) {
				t.Errorf("ParseKey(%q) = %q, %v, %v", key, gotDB, gotT, ok)
			}
		}
	}
	if got := Key("app", at, ".dump"); got != "dumps/app/2026-10-04T200102Z.dump" {
		t.Errorf("keys use UTC: got %q", got)
	}
	for _, bad := range []string{"dumps/app", "pgbackrest/x/2026-10-04T200102Z.dump", "dumps/app/notatime.dump", "dumps/a/b/2026-10-04T200102Z.dump"} {
		if _, _, ok := ParseKey(bad); ok {
			t.Errorf("ParseKey(%q) should fail", bad)
		}
	}
}
