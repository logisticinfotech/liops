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
)

const nginxSite = `# Managed by hand
server {
    listen 443 ssl;
    server_name shop.example.com www.example.com;
    access_log /var/log/nginx/shop.access.log;  # its own log
    location / { try_files $uri /index.php?$query_string; }
    location ~ \.php$ {
        access_log off; # inside a location: not the server's
        fastcgi_pass unix:/run/php/php8.3-fpm.sock;
    }
}
server {
    server_name api.example.com;
    root /srv/api/public;
}
server {
    server_name "health.example.com";
    access_log off;
}
`

func TestNginxLogs(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"sites-available", "sites-enabled", "conf.d"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	os.WriteFile(filepath.Join(dir, "sites-available", "shop"), []byte(nginxSite), 0o644)
	os.Symlink(filepath.Join(dir, "sites-available", "shop"), filepath.Join(dir, "sites-enabled", "shop"))

	plan, err := PlanNginxLogs(dir)
	if err != nil || plan.Done || len(plan.Files) != 2 {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if site := plan.Files[1]; strings.Join(site.Sites, ",") != "shop.example.com www.example.com" || len(site.Adds) != 1 {
		t.Errorf("only the server with its own access_log needs a line: %+v", site)
	}
	if len(plan.Skipped) != 1 || !strings.Contains(plan.Skipped[0], "health.example.com") {
		t.Errorf("skipped = %q", plan.Skipped)
	}

	// The config check fails: everything is put back, nothing reloaded.
	var calls []string
	fail := &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
		calls = append(calls, p+" "+strings.Join(c.Args, " "))
		if p == "nginx" {
			return []byte("nginx: [emerg] unknown directive"), errors.New("exit status 1")
		}
		return nil, nil
	}}
	if err := ApplyWebLogs(context.Background(), fail, plan, filepath.Join(dir, "backup")); err == nil || !strings.Contains(err.Error(), "put the files back") {
		t.Errorf("err = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "sites-available", "shop")); string(data) != nginxSite {
		t.Error("the site should be restored")
	}
	if _, err := os.Stat(filepath.Join(dir, "conf.d", nginxConfName)); err == nil {
		t.Error("the new conf.d file should be removed")
	}
	if strings.Contains(strings.Join(calls, "\n"), "reload") {
		t.Error("nothing should be reloaded")
	}

	calls = nil
	ok := &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
		calls = append(calls, p+" "+strings.Join(c.Args, " "))
		return nil, nil
	}}
	if err := ApplyWebLogs(context.Background(), ok, plan, filepath.Join(dir, "backup")); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, "|") != "nginx -t|systemctl reload nginx" {
		t.Errorf("calls = %q", calls)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "sites-enabled", "shop")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("sites-enabled/shop should still be a symlink")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "sites-available", "shop"))
	if strings.Count(string(data), "liops-access.log liops; # liops monitoring") != 1 ||
		!strings.Contains(string(data), "shop.access.log;  # its own log\n    access_log /var/log/nginx/liops-access.log liops; # liops monitoring\n    location") {
		t.Errorf("site:\n%s", data)
	}
	if again, _ := PlanNginxLogs(dir); !again.Done {
		t.Errorf("a second plan should find nothing to do: %+v", again)
	}
}

const caddyfile = `{
	email admin@example.com
}

(common) {
	encode gzip
}

shop.example.com {
	import common
	reverse_proxy localhost:8080
}

api.example.com {
	log {
		output file /var/log/caddy/api.log
	}
	php_fastcgi unix//run/php/php8.3-fpm.sock
}

import sites/*.caddy
`

func TestCaddyLogs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Caddyfile")
	os.WriteFile(path, []byte(caddyfile), 0o644)
	plan, err := PlanCaddyLogs(path)
	if err != nil || len(plan.Files) != 1 || strings.Join(plan.Files[0].Sites, ",") != "shop.example.com" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if len(plan.Skipped) != 1 || !strings.Contains(plan.Skipped[0], "import sites") {
		t.Errorf("skipped = %q", plan.Skipped)
	}
	var calls []string
	run := &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
		calls = append(calls, p+" "+strings.Join(c.Args, " "))
		return nil, nil
	}}
	if err := ApplyWebLogs(context.Background(), run, plan, filepath.Join(t.TempDir(), "backup")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "shop.example.com {\n\tlog # liops monitoring\n\timport common") || strings.Count(string(data), "liops monitoring") != 1 {
		t.Errorf("Caddyfile:\n%s", data)
	}
	if !strings.HasPrefix(calls[0], "caddy validate --config "+path) || calls[1] != "systemctl reload caddy" {
		t.Errorf("calls = %q", calls)
	}
}

const apacheSite = `<VirtualHost *:80>
	ServerName shop.example.com
	ServerAlias www.example.com
	DocumentRoot /srv/shop/public
	ErrorLog ${APACHE_LOG_DIR}/error.log
	CustomLog ${APACHE_LOG_DIR}/access.log combined
</VirtualHost>

<VirtualHost *:8081>
	ServerName api.example.com
	DocumentRoot /srv/api/public
</VirtualHost>
`

func TestApacheLogs(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"sites-available", "sites-enabled", "conf-available", "conf-enabled"} {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	os.WriteFile(filepath.Join(dir, "sites-available", "shop.conf"), []byte(apacheSite), 0o644)
	os.Symlink("../sites-available/shop.conf", filepath.Join(dir, "sites-enabled", "shop.conf"))

	plan, err := PlanApacheLogs(dir)
	if err != nil || len(plan.Files) != 2 || plan.Files[0].Link == "" || strings.Join(plan.Files[1].Sites, ",") != "shop.example.com www.example.com" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	fail := &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
		if p == "apache2ctl" {
			return []byte("AH00526: Syntax error"), errors.New("exit status 1")
		}
		return nil, nil
	}}
	if err := ApplyWebLogs(context.Background(), fail, plan, t.TempDir()); err == nil {
		t.Fatal("a failed configtest should stop it")
	}
	if _, err := os.Lstat(filepath.Join(dir, "conf-enabled", "liops-monitor.conf")); err == nil {
		t.Error("the conf-enabled link should be removed again")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "sites-available", "shop.conf")); string(data) != apacheSite {
		t.Error("the site should be put back")
	}
	var calls []string
	ok := &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
		calls = append(calls, p+" "+strings.Join(c.Args, " "))
		return nil, nil
	}}
	if err := ApplyWebLogs(context.Background(), ok, plan, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, "|") != "apache2ctl configtest|systemctl reload apache2" {
		t.Errorf("calls = %q", calls)
	}
	if target, err := os.Readlink(filepath.Join(dir, "conf-enabled", "liops-monitor.conf")); err != nil || target != "../conf-available/liops-monitor.conf" {
		t.Errorf("link = %q, %v", target, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "sites-available", "shop.conf"))
	if !strings.Contains(string(data), "access.log combined\n\t# liops monitoring\n\tCustomLog ${APACHE_LOG_DIR}/liops-access.log liops\n</VirtualHost>") ||
		strings.Count(string(data), "liops-access.log") != 1 {
		t.Errorf("site:\n%s", data)
	}
	if again, _ := PlanApacheLogs(dir); !again.Done {
		t.Errorf("a second plan should find nothing to do: %+v", again)
	}
}
