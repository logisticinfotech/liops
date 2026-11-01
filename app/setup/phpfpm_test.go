// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/php"
)

const poolConf = `[www]
user = www-data
listen = /run/php/php8.3-fpm.sock
pm = dynamic
pm.max_children = 5
;pm.status_path = /status

[shop]
listen = /run/php/shop.sock
pm.status_path = /fpm-status
request_slowlog_timeout = 5s
slowlog = /var/log/shop.slow.log
`

func TestPHPFPM(t *testing.T) {
	etc := t.TempDir()
	dir := filepath.Join(etc, "8.3", "fpm", "pool.d")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "www.conf"), []byte(poolConf), 0o644)
	old := php.OPcacheScript
	php.OPcacheScript = filepath.Join(t.TempDir(), "lib", "opcache-status.php")
	t.Cleanup(func() { php.OPcacheScript = old })

	plan, err := PlanPHPFPM(etc)
	if err != nil || plan.Done || !plan.Script || len(plan.Files) != 1 || strings.Join(plan.Versions, ",") != "8.3" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if f := plan.Files[0]; strings.Join(f.Sites, ",") != "8.3/www" || len(f.Adds) != 3 {
		t.Errorf("only www needs settings: %+v", f)
	}

	fail := &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
		if p == "php-fpm8.3" {
			return []byte("ERROR: [pool www] unknown entry"), errors.New("exit status 78")
		}
		return nil, nil
	}}
	if err := ApplyPHPFPM(context.Background(), fail, plan, t.TempDir()); err == nil || !strings.Contains(err.Error(), "put the files back") {
		t.Errorf("err = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "www.conf")); string(data) != poolConf {
		t.Error("the pool file should be restored")
	}

	var calls []string
	ok := &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
		calls = append(calls, p+" "+strings.Join(c.Args, " "))
		return nil, nil
	}}
	if err := ApplyPHPFPM(context.Background(), ok, plan, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, "|") != "php-fpm8.3 -t|systemctl reload php8.3-fpm" {
		t.Errorf("calls = %q", calls)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "www.conf"))
	want := ";pm.status_path = /status\n\n; liops monitoring\npm.status_path = /liops-fpm-status\nrequest_slowlog_timeout = 3s\nslowlog = /var/log/php8.3-fpm.www.slow.log\n\n[shop]"
	if !strings.Contains(string(data), want) {
		t.Errorf("www.conf:\n%s", data)
	}
	if script, _ := os.ReadFile(php.OPcacheScript); string(script) != php.OPcacheScriptSource {
		t.Error("the OPcache script should be installed")
	}
	pools, _ := php.FindPools(etc)
	if pools[0].StatusPath != php.StatusPath || !pools[0].SlowLogOn() || pools[1].StatusPath != "/fpm-status" {
		t.Errorf("pools after = %+v", pools)
	}
	if again, _ := PlanPHPFPM(etc); !again.Done {
		t.Errorf("a second plan should find nothing to do: %+v", again)
	}
}
