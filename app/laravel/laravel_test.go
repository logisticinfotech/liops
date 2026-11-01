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
	if c.QueueKey != "" {
		t.Errorf("sync has no queue key: %q", c.QueueKey)
	}
	if !c.SessionsInRedis || !c.SessionsShareDB || c.SessionDB != "0" || c.Database != "shop" {
		t.Errorf("sessions: %+v", c)
	}

	// Cached, sessions in their own Redis database: nothing bad.
	b := newApp(t, "APP_ENV=production\nAPP_DEBUG=false\nLOG_LEVEL=error\nQUEUE_CONNECTION=redis\nSESSION_DRIVER=redis\nCACHE_STORE=redis\nTELESCOPE_ENABLED=false\n")
	for _, f := range []string{"config.php", "routes-v7.php", "events.php"} {
		os.WriteFile(filepath.Join(b.Dir, "bootstrap", "cache", f), nil, 0o644)
	}
	c = CheckApp(b)
	if len(c.Findings) != 0 || c.SessionsShareDB || c.SessionDB != "0" {
		t.Errorf("a healthy app: %+v", c)
	}
}

func TestCheckAppMissingKeysUseLaravelDefaults(t *testing.T) {
	a := newApp(t, "APP_ENV=production\nAPP_DEBUG=false\n")
	for _, f := range []string{"config.php", "routes-v7.php", "events.php"} {
		os.WriteFile(filepath.Join(a.Dir, "bootstrap", "cache", f), nil, 0o644)
	}
	c := CheckApp(a)
	var text string
	for _, f := range c.Findings {
		text += f.Text + "\n"
	}
	if !strings.Contains(text, "LOG_LEVEL is debug") {
		t.Errorf("omitted LOG_LEVEL should use Laravel's debug default:\n%s", text)
	}
	if strings.Contains(text, "QUEUE_CONNECTION is sync") {
		t.Errorf("omitted QUEUE_CONNECTION should use Laravel 11's database default, not sync:\n%s", text)
	}

	os.MkdirAll(filepath.Join(a.Dir, "config"), 0o755)
	os.WriteFile(filepath.Join(a.Dir, "config", "queue.php"), []byte("<?php return ['default' => env('QUEUE_CONNECTION', 'sync')];\n"), 0o644)
	os.WriteFile(filepath.Join(a.Dir, "config", "logging.php"), []byte("<?php return ['level' => env('LOG_LEVEL', 'error')];\n"), 0o644)
	c = CheckApp(a)
	text = ""
	for _, f := range c.Findings {
		text += f.Text + "\n"
	}
	if !strings.Contains(text, "QUEUE_CONNECTION is sync") {
		t.Errorf("config/queue.php default sync should be a problem:\n%s", text)
	}
	if strings.Contains(text, "LOG_LEVEL is debug") {
		t.Errorf("config/logging.php default error should not flag debug:\n%s", text)
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

func TestErrorsFollowsDeployerSharedStorage(t *testing.T) {
	root := t.TempDir()
	release := filepath.Join(root, "releases", "20261101-0500")
	sharedLogs := filepath.Join(root, "shared", "storage", "logs")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sharedLogs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "shared", "storage"), filepath.Join(release, "storage")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "20261101-0500"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ts := now.Format("2006-01-02 15:04:05")
	if err := os.WriteFile(filepath.Join(sharedLogs, "laravel.log"), []byte("["+ts+"] production.ERROR: Payment gateway timeout for order 12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := App{Dir: filepath.Join(root, "current")}
	groups := Errors(a, now.Add(-time.Hour))
	if len(groups) != 1 || groups[0].Where != "Payment gateway timeout for order N" {
		t.Errorf("shared storage logs should be read: %+v", groups)
	}
}

func TestAppRootOnlyWidensADeployBase(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	link := filepath.Join(root, "app")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got := AppRoot(link); got != link {
		t.Errorf("a symlink whose parent has no releases/ or shared/ must stay the app folder, got %q", got)
	}
	if got := AppRoot(real); got != real {
		t.Errorf("a real folder is its own root, got %q", got)
	}

	deploy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(deploy, "releases", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(deploy, "current")
	if err := os.Symlink(filepath.Join("releases", "1"), current); err != nil {
		t.Fatal(err)
	}
	if got := AppRoot(current); got != deploy {
		t.Errorf("AppRoot(current) = %q, want the deploy base %q", got, deploy)
	}
}

func TestErrorsRefusesSymlinkedLogsDir(t *testing.T) {
	a := newApp(t, "")
	outside := t.TempDir()
	now := time.Now()
	ts := now.Format("2006-01-02 15:04:05")
	os.WriteFile(filepath.Join(outside, "laravel.log"), []byte("["+ts+"] production.ERROR: stolen secret\n"), 0o644)
	logs := filepath.Join(a.Dir, "storage", "logs")
	os.RemoveAll(logs)
	if err := os.Symlink(outside, logs); err != nil {
		t.Fatal(err)
	}
	if groups := Errors(a, now.Add(-time.Hour)); len(groups) != 0 {
		t.Errorf("must not follow a symlinked storage/logs: %+v", groups)
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

func TestQueueKey(t *testing.T) {
	for env, want := range map[string]string{
		"QUEUE_CONNECTION=redis\nAPP_NAME=\"Shop App\"\n":                  "shop_app_database_queues:default",
		"QUEUE_CONNECTION=redis\nREDIS_PREFIX=shop:\nREDIS_QUEUE=emails\n": "shop:queues:emails",
		"QUEUE_CONNECTION=redis\nREDIS_PREFIX=\n":                          "queues:default",
		"QUEUE_CONNECTION=redis\n":                                         "laravel_database_queues:default",
	} {
		if got := CheckApp(newApp(t, env)).QueueKey; got != want {
			t.Errorf("%q: %q, want %q", env, got, want)
		}
	}
}

func TestFindAppsApache(t *testing.T) {
	a := newApp(t, "")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "sites-enabled"), 0o755)
	os.WriteFile(filepath.Join(dir, "sites-enabled", "shop.conf"), []byte("<VirtualHost *:80>\n\tServerName shop.example.com\n\tServerAlias *.shop.example.com\n\tDocumentRoot \""+a.Dir+"/public\"\n</VirtualHost>\n<VirtualHost *:80>\n\tServerName static.example.com\n\tDocumentRoot /var/www/html\n</VirtualHost>\n"), 0o644)
	old := ApacheDir
	ApacheDir = dir
	t.Cleanup(func() { ApacheDir = old })
	apps := FindApps(t.TempDir(), filepath.Join(dir, "none"))
	if len(apps) != 1 || apps[0].Dir != a.Dir || strings.Join(apps[0].Sites, ",") != "*.shop.example.com,shop.example.com" {
		t.Errorf("apps = %+v", apps)
	}
}
