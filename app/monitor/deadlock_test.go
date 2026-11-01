// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"strings"
	"testing"
)

const innodbStatus = `
=====================================
2026-11-01 18:00:00 0x7f2a1c0e6700 INNODB MONITOR OUTPUT
=====================================
Per second averages calculated from the last 30 seconds
------------------------
LATEST DETECTED DEADLOCK
------------------------
2026-11-01 17:59:58 0x7f2a1c0e6700
*** (1) TRANSACTION:
TRANSACTION 5634, ACTIVE 5 sec starting index read
mysql tables in use 1, locked 1
LOCK WAIT 3 lock struct(s), heap size 1128, 2 row lock(s)
MySQL thread id 12, OS thread handle 1394, query id 300 localhost app updating
UPDATE customers SET name = 'Someone' WHERE id = 2
*** (1) HOLDS THE LOCK(S):
RECORD LOCKS space id 2 page no 4 n bits 72 index PRIMARY of table ` + "`tenant_a`.`customers`" + ` trx id 5634 lock_mode X locks rec but not gap
*** (2) TRANSACTION:
TRANSACTION 5635, ACTIVE 3 sec starting index read
mysql tables in use 1, locked 1
3 lock struct(s), heap size 1128, 2 row lock(s)
MySQL thread id 13, OS thread handle 1395, query id 301 localhost app updating
UPDATE orders SET status = 'paid'
  WHERE customer_id = 1
*** (2) HOLDS THE LOCK(S):
RECORD LOCKS space id 3 page no 4 n bits 72 index PRIMARY of table ` + "`tenant_a`.`orders`" + ` trx id 5635 lock_mode X
*** WE ROLL BACK TRANSACTION (2)
------------
TRANSACTIONS
------------
Trx id counter 5640
`

func TestParseDeadlock(t *testing.T) {
	d, ok := parseDeadlock(innodbStatus)
	if !ok || d.At.Format("15:04:05") != "17:59:58" || strings.Join(d.Tables, ",") != "tenant_a.customers,tenant_a.orders" || d.RolledBack != "(2)" {
		t.Fatalf("d = %+v", d)
	}
	if len(d.Statements) != 2 || d.Statements[0] != "UPDATE customers SET name = 'Someone' WHERE id = 2" || !strings.Contains(d.Statements[1], "WHERE customer_id = 1") {
		t.Errorf("statements = %q", d.Statements)
	}
	if strings.Contains(d.Text, "Trx id counter") {
		t.Error("the report should stop at the next section")
	}
	if _, ok := parseDeadlock("------------\nTRANSACTIONS\n------------\n"); ok {
		t.Error("no deadlock since the start")
	}
}
