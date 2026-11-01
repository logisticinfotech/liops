// SPDX-License-Identifier: Apache-2.0

package laravel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newApp(t *testing.T, env string) App {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"public", "bootstrap/cache", "storage/logs", "vendor/laravel/telescope", ".git/refs/heads"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	os.WriteFile(filepath.Join(dir, "artisan"), []byte("#!/usr/bin/env php\n"), 0o755)
	os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600)
	return App{Dir: dir}
}

func TestCheckApp(t *testing.T) {
	a := newApp(t, `APP_NAME="Shop"
APP_ENV=production
APP_DEBUG=true
APP_KEY=base64:c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0
DB_DATABASE=shop
DB_PASSWORD="hunter2-secret"
LOG_LEVEL=debug
QUEUE_CONNECTION=sync # for now
SESSION_DRIVER=redis
CACHE_STORE=redis
REDIS_CACHE_DB=0
STRIPE_SECRET=sk_live_example
`)
	c := CheckApp(a)
	var all []string
	for _, f := range c.Findings {
		all = append(all, f.Text)
	}
	text := strings.Join(all, "\n")
	for _, want := range []string{"APP_DEBUG is on", "config isn't cached", "routes aren't cached", "LOG_LEVEL is debug", "QUEUE_CONNECTION is sync", "Telescope is installed"} {
		if !strings.Contains(text, want) {
			t.Errorf("findings should say %q:\n%s", want, text)
		}
	}
	for _, secret := range []string{"hunter2", "sk_live", "c2VjcmV0", "base64"} {
		if strings.Contains(fmt.Sprintf("%+v", c), secret) {
			t.Errorf("a secret leaked into the check: %q", secret)
		}
	}
	if !c.SessionsInRedis || !c.SessionsShareDB || c.SessionDB != "0" || c.Database != "shop" {
		t.Errorf("sessions: %+v", c)
	}

	// Cached, sessions in their own Redis database: nothing bad.
	b := newApp(t, "APP_ENV=production\nAPP_DEBUG=false\nQUEUE_CONNECTION=redis\nSESSION_DRIVER=redis\nCACHE_STORE=redis\nTELESCOPE_ENABLED=false\n")
	for _, f := range []string{"config.php", "routes-v7.php", "events.php"} {
		os.WriteFile(filepath.Join(b.Dir, "bootstrap", "cache", f), nil, 0o644)
	}
	c = CheckApp(b)
	if len(c.Findings) != 0 || c.SessionsShareDB || c.SessionDB != "0" {
		t.Errorf("a healthy app: %+v", c)
	}
}

func TestFindApps(t *testing.T) {
	a := newApp(t, "")
	nginx := t.TempDir()
	os.MkdirAll(filepath.Join(nginx, "sites-enabled"), 0o755)
	os.WriteFile(filepath.Join(nginx, "sites-enabled", "shop"), []byte("server {\n  server_name shop.example.com www.example.com;\n  root "+a.Dir+"/public;\n}\nserver {\n  server_name static.example.com;\n  root /var/www/html;\n}\n"), 0o644)
	caddy := filepath.Join(t.TempDir(), "Caddyfile")
	os.WriteFile(caddy, []byte("api.example.com {\n\troot * "+a.Dir+"/public\n\tphp_fastcgi unix//run/php/php8.3-fpm.sock\n}\n"), 0o644)
	apps := FindApps(nginx, caddy)
	if len(apps) != 1 || apps[0].Dir != a.Dir || strings.Join(apps[0].Sites, ",") != "api.example.com,shop.example.com,www.example.com" {
		t.Errorf("apps = %+v", apps)
	}
}

func TestErrors(t *testing.T) {
	a := newApp(t, "")
	now := time.Now()
	ts := func(d time.Duration) string { return now.Add(-d).Format("2006-01-02 15:04:05") }
	log := ""
	for i := 0; i < 3; i++ {
		log += "[" + ts(time.Duration(i)*time.Minute) + "] production.ERROR: SQLSTATE[42S22]: Column not found: 1054 Unknown column 'x' {\"userId\":7,\"exception\":\"[object] (Illuminate\\\\Database\\\\QueryException(code: 42S22): SQLSTATE[42S22] at " + a.Dir + "/vendor/laravel/framework/src/Illuminate/Database/Connection.php:829)\n[stacktrace]\n#0 {main}\n\"}\n"
	}
	log += "[" + ts(time.Minute) + "] production.INFO: fine\n"
	log += "[" + ts(2*time.Minute) + "] production.CRITICAL: Payment gateway timeout for order 12345\n"
	log += "[" + ts(48*time.Hour) + "] production.ERROR: too old\n"
	os.WriteFile(filepath.Join(a.Dir, "storage", "logs", "laravel.log"), []byte(log), 0o644)
	groups := Errors(a, now.Add(-24*time.Hour))
	if len(groups) != 2 || groups[0].Count != 3 || groups[0].Class != `Illuminate\\Database\\QueryException` && groups[0].Class != `Illuminate\Database\QueryException` {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].Where != "vendor/laravel/framework/src/Illuminate/Database/Connection.php:829" || groups[1].Where != "Payment gateway timeout for order N" {
		t.Errorf("where = %q, %q", groups[0].Where, groups[1].Where)
	}
}

func TestRelease(t *testing.T) {
	a := newApp(t, "")
	os.WriteFile(filepath.Join(a.Dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.WriteFile(filepath.Join(a.Dir, ".git", "refs", "heads", "main"), []byte("9f86d081884c7d659a2feaa0c55ad015a3bf4f1b\n"), 0o644)
	if r := Release(a); r != "9f86d081884c" {
		t.Errorf("git: %q", r)
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "releases", "20261101-0500"), 0o755)
	os.Symlink(filepath.Join(root, "releases", "20261101-0500"), filepath.Join(root, "current"))
	if r := Release(App{Dir: filepath.Join(root, "current")}); r != "20261101-0500" {
		t.Errorf("symlink: %q", r)
	}
}
