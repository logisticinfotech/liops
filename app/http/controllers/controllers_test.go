// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/dumps"
	"github.com/logisticinfotech/liops/app/engines/mysqldump"
	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/engines/pgdump"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/engines/xtrabackup"
	"github.com/logisticinfotech/liops/app/jobs"
	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/service"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

type harness struct {
	t          *testing.T
	srv        *httptest.Server
	ui         *Server
	svc        *service.Service
	st         *state.Store
	store      *storage.Memory
	client     *http.Client
	configPath string
}

func validConfig() *config.Config {
	c := config.Default()
	c.Server.Name, c.Server.Timezone = "db-test", "UTC"
	c.PgBackRest.Stanza = "main"
	c.Storage.S3.Endpoint, c.Storage.S3.Bucket = "example-account.r2.cloudflarestorage.com", "example-bucket"
	c.Storage.S3.AccessKeyID, c.Storage.S3.SecretAccessKey = "key", "the-secret"
	c.Email.Username, c.Email.Password, c.Email.From = "alerts@example.com", "app-password", "alerts@example.com"
	c.Email.To = []string{"dev@example.com"}
	return c
}

// fakeGTIDMode is the fake MySQL server's gtid_mode: setup's SET GLOBAL steps change it.
var fakeGTIDMode = "OFF"

// fakePendingRestart is pg_settings.pending_restart for shared_preload_libraries.
var fakePendingRestart = "t"

// fakeServer answers like a PostgreSQL 18 server with pgBackRest and one database, or
// like a MySQL 8.4 server with XtraBackup and one database.
func fakeServer(program string, c command.Cmd) ([]byte, error) {
	args := strings.Join(c.Args, " ")
	now := time.Now().Unix()
	switch program {
	case "mysql":
		q := strings.TrimPrefix(c.Args[len(c.Args)-1], "--execute=")
		switch {
		case strings.Contains(q, "VERSION()"):
			return []byte("8.4.11-0ubuntu0.26.04.1\n"), nil
		case strings.Contains(q, "@@server_uuid"):
			return []byte("3e11fa47-71ca-11e1-9e33-c80aa9429562\n"), nil
		case strings.Contains(q, "@@log_bin,"):
			enforce := "OFF"
			if fakeGTIDMode != "OFF" {
				enforce = "ON"
			}
			return []byte("1\tROW\t" + fakeGTIDMode + "\t" + enforce + "\t2592000\n"), nil
		case strings.HasPrefix(q, "SET GLOBAL gtid_mode = "):
			fakeGTIDMode = strings.TrimPrefix(q, "SET GLOBAL gtid_mode = ")
		case strings.Contains(q, "Ongoing_anonymous_transaction_count"):
			return []byte("Ongoing_anonymous_transaction_count\t0\n"), nil
		case strings.Contains(q, "information_schema.TABLES"):
			return []byte("0\n"), nil
		case strings.Contains(q, "information_schema.SCHEMATA"):
			return []byte(strings.ToUpper(hex.EncodeToString([]byte("app"))) + "\n"), nil
		}
		return nil, nil
	case "xtrabackup":
		if strings.Contains(args, "--version") {
			return []byte("xtrabackup version 8.4.0-7 based on MySQL server 8.4.0 Linux (x86_64)\n"), nil
		}
		for _, a := range c.Args {
			if dir, ok := strings.CutPrefix(a, "--extra-lsndir="); ok {
				os.WriteFile(filepath.Join(dir, "xtrabackup_checkpoints"), []byte("from_lsn = 0\nto_lsn = 5000\n"), 0o600)
				os.WriteFile(filepath.Join(dir, "xtrabackup_info"), []byte("binlog_pos = filename 'binlog.000003', position '158'\n"), 0o600)
			}
		}
		return []byte("XBSTREAM"), nil
	case "mysqldump":
		return []byte("-- MySQL dump\n"), nil
	case "pg_lsclusters":
		return []byte("18  main    5432 online postgres /var/lib/postgresql/18/main /var/log/postgresql/postgresql-18-main.log\n"), nil
	case "pgbackrest":
		if strings.HasSuffix(args, " info") {
			return fmt.Appendf(nil, `[{"name":"main","status":{"code":0,"message":"ok"},"archive":[{"min":"000000010000000000000001","max":"000000010000000000000009"}],
				"backup":[{"label":"20261015-013001F","type":"full","timestamp":{"start":%d,"stop":%d},"info":{"size":1048576,"repository":{"delta":4096}}}]}]`, now-3636, now-3600), nil
		}
		return nil, nil
	case "psql":
		if strings.Contains(args, "pg_stat_archiver") {
			return fmt.Appendf(nil, "%d\t0\n", now), nil
		}
		if strings.Contains(args, "pending_restart") {
			return []byte(fakePendingRestart + "\n"), nil
		}
		return []byte(hex.EncodeToString([]byte("app")) + "\n"), nil
	}
	return nil, nil
}

func newHarness(t *testing.T, cfg *config.Config, setupDone bool) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, configPath: filepath.Join(dir, "config.yaml"), store: storage.NewMemory()}
	if err := cfg.Save(h.configPath); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	if setupDone {
		if err := st.SetMeta(service.MetaSetupComplete, "yes"); err != nil {
			t.Fatal(err)
		}
	}
	fake := &command.Fake{Handle: fakeServer}
	newRunner := func(cfg *config.Config) (*jobs.Runner, error) {
		r := &jobs.Runner{
			Cfg: cfg, Server: "db-test", State: st, Store: h.store, Mail: &notify.Recorder{},
			LockPath: filepath.Join(dir, "job.lock"), Now: time.Now, Log: io.Discard,
		}
		if cfg.IsMySQL() {
			r.XtraBackup = xtrabackup.New(cfg, fake, h.store, filepath.Join(dir, "xtrabackup"))
			r.Dumps = mysqldump.New(cfg, fake, h.store)
		} else {
			r.Backrest, r.Dumps = pgbackrest.New(cfg, fake), pgdump.New(cfg, fake, h.store)
			r.Cluster = postgres.Cluster{Cfg: cfg.Postgres, Run: fake}
		}
		return r, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.svc, err = service.New(ctx, h.configPath, filepath.Join(dir, "job.lock"), st, io.Discard, newRunner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.svc.Close)
	h.svc.WriteSettings = func(context.Context, *config.Config) (setup.Archiving, error) { return setup.Ready, nil }
	h.svc.SetMonitor = func(bool) error { return nil }

	h.ui, err = New(h.svc)
	if err != nil {
		t.Fatal(err)
	}
	h.ui.Cmd = fake
	h.ui.ApacheDir = filepath.Join(dir, "no-apache") // CI runners have an /etc/apache2 of their own
	h.srv = httptest.NewServer(h.ui.Handler())
	t.Cleanup(h.srv.Close)
	jar, _ := cookiejar.New(nil)
	h.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return h
}

func (h *harness) get(path string) (int, string, string) {
	h.t.Helper()
	resp, err := h.client.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// post submits a form from page `from` (to pick up its CSRF token).
func (h *harness) post(from, path string, form url.Values) (int, string, string) {
	h.t.Helper()
	_, _, body := h.get(from)
	m := csrfRE.FindStringSubmatch(body)
	if m == nil {
		h.t.Fatalf("no CSRF token on %s:\n%s", from, body)
	}
	form.Set("csrf", m[1])
	req, _ := http.NewRequest("POST", h.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", h.srv.URL) // browsers send it on form posts
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

func TestDashboardAndDownload(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	h.store.Add(pgdump.Key("app", time.Now().Add(-time.Hour)), []byte("PGDMP"), time.Now())

	code, _, body := h.get("/")
	if code != 200 || !strings.Contains(body, "Last full backup") {
		t.Fatalf("dashboard: %d\n%s", code, body)
	}
	if _, _, body := h.get("/backups"); !strings.Contains(body, "20261015-013001F") {
		t.Errorf("backups page:\n%s", body)
	}
	_, _, body = h.get("/databases")
	m := regexp.MustCompile(`name="key" value="(dumps/[^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no download button:\n%s", body)
	}
	code, loc, _ := h.post("/databases", "/download", url.Values{"key": {m[1]}})
	if code != 303 || !strings.HasPrefix(loc, "https://storage.example.com/dumps/app/") {
		t.Fatalf("download: %d %q", code, loc)
	}
	if _, _, body := h.get("/databases/app"); !strings.Contains(body, "sudo liops dumps url &#39;"+m[1]+"&#39;") ||
		!strings.Contains(body, "createdb &#39;app_restored&#39;") || strings.Contains(body, `action="/restore`) {
		t.Errorf("database page should show restore commands and no restore button:\n%s", body)
	}
	if code, _, _ := h.post("/databases", "/download", url.Values{"key": {"pgbackrest/archive/x"}}); code != 400 {
		t.Errorf("only dumps can be downloaded, got %d", code)
	}
	if code, _, _ := h.post("/databases", "/download", url.Values{"key": {"dumps/../pgbackrest/x"}}); code != 400 {
		t.Errorf("a key with .. must be refused, got %d", code)
	}
	entries, _ := h.st.AuditLog(0, 20)
	found := false
	for _, e := range entries {
		if e.Action == "download" && e.User == "ui" && e.Detail == m[1] && e.OK {
			found = true
		}
	}
	if !found {
		t.Errorf("the download should be audited: %+v", entries)
	}
	if _, _, body := h.get("/audit"); !strings.Contains(body, m[1]) {
		t.Error("the audit page should show the download")
	}
}

func TestSettingsKeepSecrets(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	_, _, body := h.get("/settings")
	if strings.Contains(body, "the-secret") || strings.Contains(body, "app-password") {
		t.Fatal("secrets must never be sent to the browser")
	}
	form := url.Values{"action": {"save"}, "endpoint": {"example-account.r2.cloudflarestorage.com"}, "region": {"auto"},
		"bucket": {"renamed-bucket"}, "access_key_id": {"key"}, "secret_access_key": {""}, "path_style": {"on"}, "verify_tls": {"on"}}
	if code, loc, body := h.post("/settings", "/settings/storage", form); code != 303 || !strings.HasPrefix(loc, "/settings") {
		t.Fatalf("save: %d %q\n%s", code, loc, body)
	}
	cfg, err := config.Load(h.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.S3.Bucket != "renamed-bucket" || cfg.Storage.S3.SecretAccessKey != "the-secret" {
		t.Errorf("bucket should change, the empty secret field keeps the secret: %+v", cfg.Storage.S3)
	}
	if h.svc.Runner().Cfg.Storage.S3.Bucket != "renamed-bucket" {
		t.Error("saved settings should be live without a restart")
	}

	form.Set("bucket", "")
	if _, _, body := h.post("/settings", "/settings/storage", form); !strings.Contains(body, "storage.s3.bucket: required") {
		t.Error("invalid settings must be refused with the reason")
	}
}

func TestRunNow(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	if code, loc, _ := h.post("/jobs", "/jobs/run", url.Values{"job": {"full"}}); code != 400 {
		t.Errorf("full can't be started from the UI: %d %q", code, loc)
	}
	if code, _, _ := h.post("/jobs", "/jobs/run", url.Values{"job": {"report"}}); code != 303 {
		t.Fatalf("run report: %d", code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if run, _ := h.st.LastRun("report", ""); run != nil && run.Status != state.StatusRunning {
			if run.Status != state.StatusOK {
				t.Errorf("report run: %+v", run)
			}
			if _, _, body := h.get(fmt.Sprintf("/jobs/%d", run.ID)); !strings.Contains(body, "report #") {
				t.Errorf("job page:\n%s", body)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the report never finished")
}

func TestProtections(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	req := httptest.NewRequest("GET", "/login", nil)
	req.Host = "attacker.example.com"
	rec := httptest.NewRecorder()
	h.ui.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("a foreign Host must be refused: %d", rec.Code)
	}

	resp, err := h.client.PostForm(h.srv.URL+"/jobs/run", url.Values{"job": {"report"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a POST without a CSRF token must be refused: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	if rp := resp.Header.Get("Referrer-Policy"); rp == "no-referrer" {
		t.Error(`Referrer-Policy no-referrer makes browsers send "Origin: null", which breaks every form`)
	}
}

func TestEveryPageRenders(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	h.store.Add(pgdump.Key("app", time.Now()), []byte("PGDMP"), time.Now())
	h.store.Add(dumps.GlobalsKey(time.Now()), []byte("CREATE ROLE"), time.Now())
	run := &state.Run{Job: "dumps", Start: time.Now(), Dumps: []state.DumpRun{{DB: "app", Size: 5}, {DB: "gone", Error: "boom"}}}
	h.st.StartRun(run)
	run.Status, run.End = state.StatusFailed, time.Now()
	h.st.SaveRun(run)
	h.st.SetDeleted("gone", time.Now())

	for _, path := range []string{"/", "/backups", "/databases", "/databases/app", "/databases/gone", "/jobs", "/jobs?job=dumps", fmt.Sprintf("/jobs/%d", run.ID), "/settings", "/audit"} {
		code, loc, body := h.get(path)
		if code != 200 {
			t.Errorf("GET %s: %d %q\n%s", path, code, loc, body)
		}
	}
	if _, _, body := h.get("/"); !strings.Contains(body, "A backup job failed") {
		t.Error("a failed dumps run should make the dashboard say FAILED")
	}
	if _, _, body := h.get("/databases"); !strings.Contains(body, "no longer found since") {
		t.Error("deleted databases should be marked")
	}
}

func TestWizardStartsWithoutLogin(t *testing.T) {
	h := newHarness(t, config.Default(), false)
	if _, loc, _ := h.get("/"); loc != "/setup" {
		t.Fatalf("before setup, / should go to the wizard, got %q", loc)
	}
	if _, loc, _ := h.get("/setup"); loc != "/setup/postgres" {
		t.Fatalf("the wizard starts at the PostgreSQL step, got %q", loc)
	}
	if _, _, body := h.get("/setup/postgres"); !strings.Contains(body, "PostgreSQL 18 / main") {
		t.Errorf("clusters missing:\n%s", body)
	}
	code, loc, _ := h.post("/setup/postgres", "/setup/postgres", url.Values{"cluster": {"0"}, "stanza": {"main"}, "name": {"db-test"}})
	if code != 303 || loc != "/setup/storage" {
		t.Fatalf("postgres step: %d %q", code, loc)
	}
	if cfg, _ := config.LoadDraft(h.configPath); cfg.PgBackRest.Stanza != "main" || cfg.Server.Name != "db-test" {
		t.Errorf("the draft should be saved: %+v", cfg.PgBackRest)
	}
	if _, _, body := h.get("/setup/review"); !strings.Contains(body, "storage.s3.bucket") {
		t.Error("review should list what's still missing")
	}
	if _, loc, _ := h.get("/settings"); loc != "/setup" {
		t.Errorf("pages wait for setup, got %q", loc)
	}
}

func TestMySQLPages(t *testing.T) {
	cfg := validConfig()
	cfg.Engine = config.EngineMySQL
	cfg.MySQL.BackupName = "shop-db-1"
	h := newHarness(t, cfg, true)
	h.store.Add(mysqldump.Key("app", time.Now().Add(-time.Hour)), []byte("gzipped SQL"), time.Now())
	stop := time.Now().Add(-2 * time.Hour)
	manifest := fmt.Sprintf(`{"label":"2026-10-25T013000Z-full","type":"full","start":%q,"stop":%q,"to_lsn":5000,"binlog_file":"binlog.000006","binlog_pos":158,"size":4900000}`,
		stop.Add(-4*time.Second).Format(time.RFC3339), stop.Format(time.RFC3339))
	h.store.Add(xtrabackup.Prefix+"2026-10-25T013000Z-full/"+xtrabackup.ManifestFile, []byte(manifest), time.Now())

	code, _, body := h.get("/")
	if code != 200 || !strings.Contains(body, "Database dumps") {
		t.Fatalf("dashboard: %d\n%s", code, body)
	}
	for _, want := range []string{"Last full backup", "4.7 MB stored", "Newest change saved (binlogs)", "Can restore back to"} {
		if !strings.Contains(body, want) {
			t.Errorf("the MySQL dashboard should show %q", want)
		}
	}
	if strings.Contains(body, "pgBackRest") {
		t.Error("the MySQL dashboard mustn't mention pgBackRest")
	}
	_, _, body = h.get("/databases/app")
	if !strings.Contains(body, "gunzip -c /tmp/liops-restore.sql.gz | sudo mysql") || strings.Contains(body, "pg_restore") {
		t.Errorf("database page should show MySQL restore commands:\n%s", body)
	}
	code, _, body = h.get("/backups")
	if code != 200 || !strings.Contains(body, "2026-10-25T013000Z-full") || !strings.Contains(body, "binlog.000006:158") {
		t.Errorf("backups page on MySQL: %d\n%s", code, body)
	}
}

// mysqlWizardConfig is a complete MySQL config, as the wizard leaves it before apply.
func mysqlWizardConfig(t *testing.T) *config.Config {
	cfg := validConfig()
	cfg.Engine = config.EngineMySQL
	cfg.MySQL.BackupName = "shop-db-1"
	cfg.MySQL.DefaultsFile = filepath.Join(t.TempDir(), "mysql.cnf")
	cfg.MySQL.ConfFile = filepath.Join(t.TempDir(), "zz-liops.cnf")
	return cfg
}

// waitTask waits for the wizard's apply task to stop running.
func (h *harness) waitTask() *applyTask {
	h.t.Helper()
	for range 200 {
		if task := h.ui.setup.snapshot(); task != nil && !task.Running {
			return task
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatal("the apply task didn't finish")
	return nil
}

func TestMySQLWizard(t *testing.T) {
	fakeGTIDMode = "OFF"
	h := newHarness(t, mysqlWizardConfig(t), false)
	if _, loc, _ := h.get("/setup"); loc != "/setup/mysql" {
		t.Fatalf("a MySQL server's wizard starts at the MySQL step, got %q", loc)
	}
	if _, _, body := h.get("/setup/mysql"); !strings.Contains(body, "MySQL root password") || strings.Contains(body, "PostgreSQL") {
		t.Errorf("MySQL step:\n%s", body)
	}
	code, loc, body := h.post("/setup/mysql", "/setup/mysql", url.Values{"backup_name": {"shop-db-2"}, "name": {"db-test"}})
	if code != 303 || loc != "/setup/storage" {
		t.Fatalf("mysql step: %d %q\n%s", code, loc, body)
	}
	if _, _, body := h.get("/setup/review"); !strings.Contains(body, "ask you before turning on GTIDs") || !strings.Contains(body, "shop-db-2") {
		t.Errorf("review:\n%s", body)
	}

	// Apply stops and asks before turning GTIDs on.
	h.post("/setup/review", "/setup/apply/start", url.Values{})
	task := h.waitTask()
	if task.Steps[0].Status != stepWaiting || !slices.Contains(task.GTID, "gtid_mode: OFF → ON") || fakeGTIDMode != "OFF" {
		t.Fatalf("apply should wait for approval with nothing changed: %+v (gtid_mode %s)", task, fakeGTIDMode)
	}
	if _, _, body := h.get("/setup/apply/progress"); !strings.Contains(body, "Turn on GTIDs now") {
		t.Errorf("progress page should offer the GTID button:\n%s", body)
	}

	// Approved: GTIDs on, setup complete, first backup and the email.
	h.post("/setup/apply/progress", "/setup/apply/gtid", url.Values{})
	task = h.waitTask()
	for i, st := range task.Steps {
		if st.Status != stepDone {
			t.Errorf("step %d %q: %s %s", i, st.Title, st.Status, st.Detail)
		}
	}
	if fakeGTIDMode != "ON" || !h.svc.SetupComplete() {
		t.Errorf("gtid_mode %s, setup complete %v", fakeGTIDMode, h.svc.SetupComplete())
	}
	if objs, _ := h.store.List(context.Background(), xtrabackup.Prefix); len(objs) != 2 {
		t.Errorf("the first full backup should be stored: %v", objs)
	}
}

func TestMySQLScheduleSettings(t *testing.T) {
	h := newHarness(t, mysqlWizardConfig(t), true)
	_, _, body := h.get("/settings?tab=backups")
	if !strings.Contains(body, `name="binlog_interval"`) || strings.Contains(body, `name="archive_timeout"`) {
		t.Fatalf("a MySQL server's schedules form has the binlog interval, not WAL:\n%s", body)
	}
	code, _, body := h.post("/settings", "/settings/schedules", url.Values{
		"full_days": {"0"}, "full_time": {"02:15"}, "diff_days": {"1-6"}, "diff_time": {"02:15"},
		"report_time": {"07:30"}, "report_days": {"*"}, "retention_days": {"14"}, "binlog_interval": {"15m"},
	})
	if code != 303 {
		t.Fatalf("save: %d\n%s", code, body)
	}
	cfg, _ := config.LoadDraft(h.configPath)
	if cfg.MySQL.FullSchedule != "15 2 * * 0" || cfg.MySQL.RetentionDays != 14 || cfg.MySQL.BinlogInterval.String() != "15m" {
		t.Errorf("saved: full %q, retention %d, binlog interval %s", cfg.MySQL.FullSchedule, cfg.MySQL.RetentionDays, cfg.MySQL.BinlogInterval)
	}
	if cfg.PgBackRest.RetentionDays != 30 {
		t.Errorf("PostgreSQL settings must be left alone: %+v", cfg.PgBackRest)
	}
}

func TestSettingsTabs(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	for tab, want := range map[string]string{
		"":        `action="/settings/server"`,
		"server":  `action="/settings/server"`,
		"storage": `action="/settings/storage"`,
		"email":   `action="/settings/email"`,
		"backups": `action="/settings/schedules"`,
		"dumps":   `action="/settings/labels"`,
		"nope":    `action="/settings/server"`,
	} {
		_, _, body := h.get("/settings?tab=" + tab)
		if !strings.Contains(body, want) || strings.Count(body, `<form method="post" action="/settings/`) > 2 {
			t.Errorf("tab %q should show only its own forms, with %s", tab, want)
		}
	}
	code, loc, _ := h.post("/settings?tab=email", "/settings/email", url.Values{
		"smtp_host": {"smtp.example.com"}, "smtp_port": {"587"}, "tls": {"starttls"},
		"username": {"alerts@example.com"}, "from": {"alerts@example.com"}, "to": {"dev@example.com"}, "action": {"save"},
	})
	if code != 303 || loc != "/settings?tab=email" {
		t.Errorf("a save should return to its tab: %d %q", code, loc)
	}
}

func TestErrorPageLeadsWithTheReason(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	h.store.FailList = "dumps/"
	_, _, body := h.get("/databases")
	if !strings.Contains(body, "Likely reason:") || !strings.Contains(body, "Technical details") || strings.Contains(body, "javascript:") {
		t.Errorf("error page:\n%s", body)
	}
}

// fakeMonitor serves canned answers on a Unix socket, as liops-monitor would.
func fakeMonitor(t *testing.T) *monitor.Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lm") // a short path: socket paths are limited to ~100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	now := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(monitor.Latest{Time: now, Values: map[string]float64{
			"cpus": 4, "cpu.busy": 37, "cpu.user": 30, "load.1": 6.5, "mem.total": 8 << 30, "mem.used": 3 << 30,
			"disk.pct:/": 41, "disk.pct:/var": 88, "io.read:sda": 1000, "net.rx:eth0": 500,
			"disk.used:/var": 88e9, "disk.size:/var": 100e9, "db.size:shop": 5 << 30, "disk.use:pg_wal": 1 << 30,
			"mem.available": 2 << 30, "redis.memory": 1e9, "redis.maxmemory": 1 << 30, "redis.keys:db0": 1200, "fpm.total:8.3/www": 5, "fpm.active:8.3/www": 5, "fpm.idle:8.3/www": 0, "fpm.queue:8.3/www": 2,
			"opcache.enabled:8.3": 1, "opcache.full:8.3": 1, "opcache.memory_pct:8.3": 100, "opcache.memory:8.3": 128 << 20, "opcache.hit_rate:8.3": 91,
			"svc.mem:nginx.service": 50 << 20, "svc.cpu:nginx.service": 2, "svc.mem:postgresql.service": 900 << 20,
		}, Processes: []monitor.Process{{Command: "php-fpm: pool www", Count: 5, CPU: 80, Memory: 5 * 60 << 20}},
			Checks: monitor.Checks{Time: now, FailedUnits: []string{"queue-worker.service"}, RebootRequired: true, RebootPackages: []string{"libc6"}}})
	})
	mux.HandleFunc("GET /v1/db", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]monitor.DBStatus{{Label: "mysql", Title: "MySQL", Engine: "mysql", Checked: now, Statements: true}})
	})
	search := monitor.QueryTotal{
		QueryShape: monitor.QueryShape{Server: "mysql", DB: "tenant_a", ID: strings.Repeat("ab", 32), FirstSeen: now.Add(-48 * time.Hour), MaxMS: 2900,
			Text: "SELECT * FROM `customers` WHERE LOWER ( `name` ) LIKE ? OR LOWER ( `email` ) LIKE ?"},
		QueryCounters: monitor.QueryCounters{Calls: 40, TimeMS: 106000, RowsExamined: 40 * 1.65e6, RowsSent: 40},
	}
	filter := monitor.QueryTotal{
		QueryShape:    monitor.QueryShape{Server: "mysql", DB: "tenant_a", ID: strings.Repeat("cd", 32), FirstSeen: now.Add(-2 * time.Hour), Text: "SELECT `id` FROM `readings` WHERE DATE ( `created_at` ) = ?"},
		QueryCounters: monitor.QueryCounters{Calls: 30, TimeMS: 54000, RowsExamined: 30 * 900000, RowsSent: 30 * 50, NoIndex: 30},
	}
	lookup := monitor.QueryTotal{
		QueryShape:    monitor.QueryShape{Server: "mysql", DB: "tenant_b", ID: strings.Repeat("ef", 32), Text: "SELECT * FROM `users` WHERE `id` = ?"},
		QueryCounters: monitor.QueryCounters{Calls: 90000, TimeMS: 9000, RowsExamined: 90000, RowsSent: 90000},
	}
	mux.HandleFunc("GET /v1/db/queries", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]monitor.QueryTotal{search, filter, lookup})
	})
	mux.HandleFunc("GET /v1/db/query", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != search.ID {
			http.Error(w, "no such query", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(monitor.QueryDetailResponse{Shape: search.QueryShape, Points: []monitor.QueryPoint{
			{At: now.Unix() - 600, QueryCounters: monitor.QueryCounters{Calls: 20, TimeMS: 53000, RowsExamined: 33e6, RowsSent: 20}},
			{At: now.Unix() - 300, QueryCounters: monitor.QueryCounters{Calls: 20, TimeMS: 53000, RowsExamined: 33e6, RowsSent: 20}},
		}})
	})
	mux.HandleFunc("GET /v1/db/deadlock", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(monitor.Deadlock{At: now, Tables: []string{"tenant_a.customers"}, RolledBack: "(2)",
			Statements: []string{"UPDATE customers SET name = 'Someone' WHERE id = 2", "UPDATE customers SET name = 'Other' WHERE id = 1"}, Text: "LATEST DETECTED DEADLOCK"})
	})
	mux.HandleFunc("GET /v1/db/sample", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(monitor.QuerySample{Text: "SELECT * FROM customers WHERE LOWER(name) LIKE '%example-term%'", Seen: now, MS: 2650})
	})
	mux.HandleFunc("GET /v1/db/activity", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(monitor.Activity{Statements: []monitor.RunningStatement{{PID: 42, DB: "tenant_a", State: "executing", Seconds: 3, BlockedBy: "41", Query: "UPDATE readings SET v = 1"}},
			Transactions: []monitor.OpenTransaction{{PID: 41, Seconds: 95, State: "RUNNING", RowsModified: 1200}}})
	})
	mux.HandleFunc("GET /v1/db/tables", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(monitor.DBTables{Server: "mysql", At: now, StatsSince: now.Add(-30 * 24 * time.Hour),
			Tables: []monitor.TableInfo{{DB: "tenant_a", Name: "customers", Rows: 1.65e6, Bytes: 400e6, PrimaryKey: true}, {DB: "tenant_a", Name: "readings", Rows: 9e5, Bytes: 600e6}},
			Indexes: []monitor.IndexInfo{{DB: "tenant_a", Table: "customers", Name: "PRIMARY", Definition: "(id)", Primary: true, Unique: true},
				{DB: "tenant_a", Table: "customers", Name: "old_idx", Definition: "(tenant_id)", Unused: true, Duplicate: "tenant_email"}},
			Memory: map[string]float64{"innodb_buffer_pool_size": 128 << 20},
			Growth: map[string]float64{"tenant_a/readings": 30e6}})
	})
	mux.HandleFunc("GET /v1/redis", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(monitor.RedisResponse{
			Status: &monitor.RedisStatus{Addr: "127.0.0.1:6379", Version: "7.2.4", Checked: now, Policy: "allkeys-lru"},
		})
	})
	mux.HandleFunc("GET /v1/requests", func(w http.ResponseWriter, r *http.Request) {
		var slow, fast monitor.RouteStats
		slow.Count, slow.TimeMS, slow.C5xx, slow.UpMS, slow.UpCount = 40, 40*2400, 3, 40*2300, 40
		slow.Hist[13] = 40 // 2–3 s
		fast.Count, fast.TimeMS = 9000, 9000*35
		fast.Hist[2] = 9000
		res := monitor.RequestsResponse{
			Sources: []monitor.LogSource{{Format: monitor.FormatNginx, Path: monitor.NginxLogPath}},
			Routes: []monitor.RouteTotal{{Host: "b.example.com", Route: "GET /dashboard", RouteStats: slow},
				{Host: "a.example.com", Route: "GET /api/readings/{n}", RouteStats: fast}},
			Hosts:   []monitor.RouteTotal{{Host: "b.example.com", RouteStats: slow}, {Host: "a.example.com", RouteStats: fast}},
			Slowest: []monitor.SlowRequest{{At: now, Host: "b.example.com", Route: "GET /dashboard", Status: 200, MS: 2950, UpstreamMS: 2900}},
			Clients: []monitor.ClientStats{{IP: "203.0.113.50", Class: "script or app", Count: 7000, TimeMS: 245000}},
			Heatmap: []monitor.HeatCell{{At: now.Truncate(time.Hour).Unix(), Count: 9040, Slow: 40}},
		}
		json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("GET /v1/series", func(w http.ResponseWriter, r *http.Request) {
		// Twelve hours in which everything grew by 1 GB: 2 GB a day.
		zero, gb := 0.0, 1e9
		res := monitor.SeriesResponse{Step: 60, X: []int64{now.Unix() - 12*3600, now.Unix()}, Series: map[string]monitor.SeriesValues{}}
		for _, n := range r.URL.Query()["name"] {
			res.Series[n] = monitor.SeriesValues{Avg: []*float64{&zero, &gb}, Max: []*float64{&zero, &gb}}
		}
		json.NewEncoder(w).Encode(res)
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socket := filepath.Join(dir, "m.sock")
	go monitor.Serve(ctx, socket, mux)
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return monitor.NewClient(socket)
}

func TestResourcesPage(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)
	for _, job := range []string{"full", "dumps"} {
		run := &state.Run{Job: job, Start: time.Now().Add(-2 * time.Hour)}
		h.st.StartRun(run)
		run.Status, run.End = state.StatusOK, run.Start.Add(5*time.Minute)
		h.st.SaveRun(run)
	}

	code, _, body := h.get("/monitoring/resources?range=7d")
	if code != 200 {
		t.Fatalf("GET: %d\n%s", code, body)
	}
	for _, want := range []string{`href="/monitoring/resources">Resources`, "6.50", "work is waiting", "/var", "88%",
		"postgresql", "php-fpm: pool www", `class="tab active" href="/monitoring/resources?range=7d"`, "data-chart=", "Disk I/O", "read sda",
		"1 service has failed", "systemctl status queue-worker.service", "needs a restart", "libc6",
		"database shop", "WAL (pg_wal)", "&#43;1.9 GB", "full in about 6 days", "Databases and logs on disk", "&#34;markers&#34;", "dumps"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page should contain %q", want)
		}
	}
	if strings.Index(body, "postgresql") > strings.Index(body, "nginx") {
		t.Error("services should be sorted by memory, largest first")
	}
}

func TestResourcesWithoutTheMonitor(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	h.ui.Monitor = monitor.NewClient("/tmp/liops-no-such.sock")
	if _, _, body := h.get("/"); strings.Contains(body, "/monitoring/resources") {
		t.Error("the sidebar shouldn't link to monitoring while it's off")
	}
	if _, _, body := h.get("/monitoring/resources"); !strings.Contains(body, "Monitoring is off") {
		t.Errorf("off: %s", body)
	}

	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h = newHarness(t, cfg, true)
	h.ui.Monitor = monitor.NewClient("/tmp/liops-no-such.sock")
	if _, _, body := h.get("/monitoring/resources"); !strings.Contains(body, "systemctl status liops-monitor") {
		t.Errorf("not running: %s", body)
	}
}

func TestMonitoringSetting(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	var calls []bool
	h.svc.SetMonitor = func(on bool) error { calls = append(calls, on); return nil }
	save := func(form url.Values) {
		form.Set("action", "save")
		if code, loc, body := h.post("/settings?tab=monitoring", "/settings/monitor", form); code != 303 || loc != "/settings?tab=monitoring" {
			t.Fatalf("save: %d %q\n%s", code, loc, body)
		}
	}
	save(url.Values{"monitor_enabled": {"on"}})
	save(url.Values{"monitor_enabled": {"on"}}) // unchanged: no second start
	if !h.svc.Config().Monitor.Enabled {
		t.Error("monitoring should be on")
	}
	save(url.Values{})
	if !slices.Equal(calls, []bool{true, false}) {
		t.Errorf("SetMonitor calls: %v", calls)
	}

	h.svc.SetMonitor = func(bool) error { return errors.New("Unit liops-monitor.service not found") }
	if _, _, body := h.post("/settings?tab=monitoring", "/settings/monitor", url.Values{"monitor_enabled": {"on"}, "action": {"save"}}); h.svc.Config().Monitor.Enabled {
		t.Errorf("a failed start shouldn't save the setting:\n%s", body)
	}
}

func TestMonitoringDatabaseAccess(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)
	_, _, body := h.get("/settings?tab=monitoring")
	for _, want := range []string{"Set up database access", "PostgreSQL needs one restart", "sudo systemctl restart postgresql@18-main", "MySQL", "collected"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Monitoring tab should show %q", want)
		}
	}

	// MySQL: the button creates the monitoring account as MySQL's root.
	mcfg := mysqlWizardConfig(t)
	mcfg.Monitor.Enabled = true
	h = newHarness(t, mcfg, true)
	h.ui.Monitor = fakeMonitor(t)
	code, loc, body := h.post("/settings?tab=monitoring", "/settings/monitor/databases", url.Values{})
	if code != 303 || loc != "/settings?tab=monitoring" {
		t.Fatalf("set up: %d %q\n%s", code, loc, body)
	}
	if _, _, body := h.get("/settings?tab=monitoring"); !strings.Contains(body, "Done.") || strings.Contains(body, "needs one restart") {
		t.Errorf("after set up:\n%s", body)
	}
	found := false
	for _, c := range h.ui.Cmd.(*command.Fake).Calls {
		found = found || strings.Contains(strings.Join(c.Args, " "), "--execute=source ")
	}
	if !found {
		t.Error("the account script should have run")
	}
}

func TestDatabasePages(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)

	code, _, body := h.get("/monitoring/database?range=24h")
	if code != 200 {
		t.Fatalf("GET: %d\n%s", code, body)
	}
	for _, want := range []string{`href="/monitoring/database">Database`, "These queries scan whole tables", "LOWER(name) in a filter",
		"DATE(created_at) in a filter", "1.65M rows", "1.8 min", "waits for 41", "Open transactions", "tenant_a.readings",
		"&#43;29 MB", "New in this range", "Never used", "Latest deadlock", "rolled back transaction (2)", "Transaction (2):", "old_idx", "Duplicates", "Tables without a primary key", "128 MB is Ubuntu&#39;s default", "About 13% of the data fits"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Database page should contain %q", want)
		}
	}
	scans := body[strings.Index(body, "These queries scan whole tables"):strings.Index(body, "Queries by total time")]
	if strings.Contains(scans, "users") {
		t.Error("a lookup by primary key isn't a scan")
	}

	code, _, body = h.get("/monitoring/database/query?server=mysql&db=tenant_a&id=" + strings.Repeat("ab", 32) + "&range=24h")
	if code != 200 {
		t.Fatalf("GET query: %d\n%s", code, body)
	}
	for _, want := range []string{"It scans whole tables", "1.65M rows read for every row it returns", "A real statement", "example-term",
		"a leading wildcard", "never stored", "old_idx", "primary key", "data-chart="} {
		if !strings.Contains(body, want) {
			t.Errorf("the query page should contain %q", want)
		}
	}
	if code, _, body := h.get("/monitoring/database/query?server=mysql&db=tenant_a&id=nope"); code != 200 || !strings.Contains(body, "isn&#39;t in the statistics") {
		t.Errorf("an unknown query: %d", code)
	}
}

func TestRequestsPage(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)
	etc := t.TempDir()
	h.ui.PHPEtc = etc
	os.MkdirAll(filepath.Join(etc, "8.3", "fpm", "pool.d"), 0o755)
	slow := filepath.Join(t.TempDir(), "www.slow.log")
	os.WriteFile(slow, []byte("\n["+time.Now().Add(-time.Second).Format("02-Jan-2006 15:04:05")+"]  [pool www] pid 4242\nscript_filename = /srv/app/public/index.php\n[0x00007f] curl_exec() /srv/app/app/Gateway.php:88\n"), 0o644)
	os.WriteFile(filepath.Join(etc, "8.3", "fpm", "pool.d", "www.conf"), []byte("[www]\nlisten = /run/php/php8.3-fpm.sock\nrequest_slowlog_timeout = 1s\nslowlog = "+slow+"\n"), 0o644)
	code, _, body := h.get("/monitoring/requests?range=24h")
	if code != 200 {
		t.Fatalf("GET: %d\n%s", code, body)
	}
	for _, want := range []string{`href="/monitoring/requests">Requests`, "Sites", "b.example.com", "GET /dashboard", "GET /api/readings/{n}",
		"3 5xx", "2.9 s", "203.0.113.50", "curl_exec() /srv/app/app/Gateway.php:88", "script or app", `class="heat-5"`, "liops-access.log", "Routes by total time"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Requests page should contain %q", want)
		}
	}
	if strings.Index(body, "GET /dashboard") > strings.Index(body, "GET /api/readings") {
		t.Error("routes should be ranked by total time")
	}
}

func TestWebServerSettings(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)
	dir := t.TempDir()
	h.ui.NginxDir, h.ui.Caddyfile, h.ui.WebBackupDir = dir, filepath.Join(dir, "no-Caddyfile"), filepath.Join(dir, "backup")
	os.MkdirAll(filepath.Join(dir, "sites-enabled"), 0o755)
	os.MkdirAll(filepath.Join(dir, "conf.d"), 0o755)
	os.WriteFile(filepath.Join(dir, "sites-enabled", "shop"), []byte("server {\n  server_name shop.example.com;\n  access_log /var/log/nginx/shop.log;\n}\n"), 0o644)

	_, _, body := h.get("/settings?tab=monitoring")
	for _, want := range []string{"Web servers", "Apply the nginx changes", "shop.example.com", "liops-monitor.conf", "nginx -t"} {
		if !strings.Contains(body, want) {
			t.Errorf("the panel should show %q", want)
		}
	}
	code, loc, body := h.post("/settings?tab=monitoring", "/settings/monitor/weblogs", url.Values{"server": {"nginx"}})
	if code != 303 || loc != "/settings?tab=monitoring" {
		t.Fatalf("apply: %d %q\n%s", code, loc, body)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "sites-enabled", "shop")); !strings.Contains(string(data), "liops-access.log liops;") {
		t.Errorf("site:\n%s", data)
	}
	if _, _, body := h.get("/settings?tab=monitoring"); !strings.Contains(body, "badge ok\">ready") {
		t.Error("after applying, nginx should show as ready")
	}
	var ran []string
	for _, c := range h.ui.Cmd.(*command.Fake).Calls {
		ran = append(ran, c.Name+" "+strings.Join(c.Args, " "))
	}
	if !slices.Contains(ran, "nginx -t") || !slices.Contains(ran, "systemctl reload nginx") {
		t.Errorf("ran %q", ran)
	}
}

func TestPHPPage(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)
	etc := t.TempDir()
	h.ui.PHPEtc = etc
	os.MkdirAll(filepath.Join(etc, "8.3", "fpm", "pool.d"), 0o755)
	slow := filepath.Join(t.TempDir(), "www.slow.log")
	os.WriteFile(slow, []byte("\n[01-Nov-2026 05:00:00]  [pool www] pid 4242\nscript_filename = /srv/app/public/index.php\n[0x00007f] usleep() /srv/app/app/Reports.php:42\n"), 0o644)
	os.WriteFile(filepath.Join(etc, "8.3", "fpm", "pool.d", "www.conf"), []byte("[www]\nlisten = /run/php/php8.3-fpm.sock\npm = dynamic\npm.max_children = 5\npm.status_path = /liops-fpm-status\nrequest_slowlog_timeout = 3s\nslowlog = "+slow+"\n"), 0o644)

	code, _, body := h.get("/monitoring/php?range=24h")
	if code != 200 {
		t.Fatalf("GET: %d\n%s", code, body)
	}
	for _, want := range []string{`href="/monitoring/php">PHP`, "Pool <span class=\"mono\">8.3/www</span>", "It ran out of workers", "Raise pm.max_children (now 5); the memory free allows about 31",
		"2 requests are waiting", "60 MB", "Reports.php:42", "OPcache is full", "Only 91.0%"} {
		if !strings.Contains(body, want) {
			t.Errorf("the PHP page should contain %q", want)
		}
	}

	// Settings: the pool already has both, but the OPcache script isn't installed.
	_, _, body = h.get("/settings?tab=monitoring")
	if !strings.Contains(body, "PHP-FPM") || !strings.Contains(body, "opcache-status.php") {
		t.Errorf("the settings panel should offer the OPcache script")
	}
}

func TestAppsAndRedisPages(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)
	app := t.TempDir()
	for _, d := range []string{"public", "bootstrap/cache", "storage/logs"} {
		os.MkdirAll(filepath.Join(app, d), 0o755)
	}
	os.WriteFile(filepath.Join(app, "artisan"), nil, 0o755)
	os.WriteFile(filepath.Join(app, ".env"), []byte("APP_ENV=production\nAPP_DEBUG=false\nDB_PASSWORD=hunter2-secret\nQUEUE_CONNECTION=sync\nSESSION_DRIVER=redis\nCACHE_STORE=redis\nREDIS_CACHE_DB=0\n"), 0o600)
	app2 := filepath.Join(t.TempDir(), "jobs")
	os.MkdirAll(filepath.Join(app2, "public"), 0o755)
	os.WriteFile(filepath.Join(app2, "artisan"), nil, 0o755)
	os.WriteFile(filepath.Join(app2, ".env"), []byte("APP_NAME=Jobs\nQUEUE_CONNECTION=redis\n"), 0o600)
	os.WriteFile(filepath.Join(app, "storage", "logs", "laravel.log"), []byte("["+time.Now().Format("2006-01-02 15:04:05")+"] production.ERROR: Payment gateway timeout for order 12345\n"), 0o644)
	nginx := t.TempDir()
	os.MkdirAll(filepath.Join(nginx, "sites-enabled"), 0o755)
	os.WriteFile(filepath.Join(nginx, "sites-enabled", "shop"), []byte("server {\n  server_name shop.example.com;\n  root "+app+"/public;\n}\nserver {\n  server_name jobs.example.com;\n  root "+app2+"/public;\n}\n"), 0o644)
	h.ui.NginxDir, h.ui.Caddyfile = nginx, filepath.Join(nginx, "none")

	code, _, body := h.get("/monitoring/apps")
	if code != 200 {
		t.Fatalf("GET apps: %d\n%s", code, body)
	}
	for _, want := range []string{`href="/monitoring/apps">Apps`, "shop.example.com", "QUEUE_CONNECTION is sync", "config isn&#39;t cached",
		"share Redis database 0", "can evict session keys", "never saves to disk", "can evict queued jobs", "jobs_database_queues:default", "Payment gateway timeout for order N", "schedule:run didn&#39;t run"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Apps page should contain %q", want)
		}
	}
	if strings.Contains(body, "hunter2") {
		t.Error("a .env secret reached the page")
	}

	code, _, body = h.get("/monitoring/redis?range=24h")
	if code != 200 {
		t.Fatalf("GET redis: %d\n%s", code, body)
	}
	for _, want := range []string{"Redis evicted", "allkeys-lru", "never", "db0", "can evict session keys"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Redis page should contain %q", want)
		}
	}
	if _, _, body := h.get("/settings?tab=monitoring"); !strings.Contains(body, "Redis 7.2.4") {
		t.Error("Settings should show Redis as ready")
	}
}

func TestOverviewPage(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)
	h.ui.PHPEtc = t.TempDir()
	code, _, body := h.get("/monitoring")
	if code != 200 {
		t.Fatalf("GET: %d\n%s", code, body)
	}
	// The fake's numbers are extreme (every series climbs to 1e9): plenty to say.
	for _, want := range []string{`href="/monitoring">Overview`, "Most likely:", "What to do:", "Details →", "Fix the app first"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Overview should contain %q", want)
		}
	}
	if lines := h.ui.VerdictLines(context.Background()); len(lines) < 3 || !strings.HasPrefix(lines[0], "Verdict: most likely,") {
		t.Errorf("verdict lines = %q", lines)
	}
	h.ui.Monitor = monitor.NewClient("/tmp/liops-no-such.sock")
	if lines := h.ui.VerdictLines(context.Background()); lines != nil {
		t.Errorf("no monitor, no verdict: %q", lines)
	}
	if _, _, body := h.get("/monitoring"); !strings.Contains(body, "isn&#39;t answering") {
		t.Error("the overview should say when the monitor isn't running")
	}
}

func TestLogsPage(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)
	app := t.TempDir()
	os.MkdirAll(filepath.Join(app, "public"), 0o755)
	os.MkdirAll(filepath.Join(app, "storage", "logs"), 0o755)
	os.WriteFile(filepath.Join(app, "artisan"), nil, 0o755)
	logFile := filepath.Join(app, "storage", "logs", "laravel.log")
	os.WriteFile(logFile, []byte("[2026-11-01 10:00:00] production.INFO: started\n[2026-11-01 10:00:01] production.ERROR: Payment gateway timeout\n[2026-11-01 10:00:02] production.INFO: gateway retry ok\n"), 0o644)
	nginx := t.TempDir()
	os.MkdirAll(filepath.Join(nginx, "sites-enabled"), 0o755)
	os.WriteFile(filepath.Join(nginx, "sites-enabled", "shop"), []byte("server {\n  server_name shop.example.com;\n  root "+app+"/public;\n}\n"), 0o644)
	h.ui.NginxDir, h.ui.Caddyfile = nginx, filepath.Join(nginx, "none")

	_, _, body := h.get("/monitoring/logs")
	for _, want := range []string{`href="/monitoring/logs">Logs`, "nginx", "postgresql", logFile} {
		if !strings.Contains(body, want) {
			t.Errorf("the source list should offer %q", want)
		}
	}
	_, _, body = h.get("/monitoring/logs?" + url.Values{"source": {"file:" + logFile}, "q": {"gateway"}, "errors": {"1"}}.Encode())
	if !strings.Contains(body, "Payment gateway timeout") || strings.Contains(body, "retry ok") || strings.Contains(body, "started") {
		t.Errorf("filtered output:\n%s", body)
	}
	if _, _, body := h.get("/monitoring/logs?" + url.Values{"source": {"file:/etc/shadow"}}.Encode()); !strings.Contains(body, "isn&#39;t one liops shows") {
		t.Error("only listed logs may be read")
	}
	_, _, body = h.get("/monitoring/logs?" + url.Values{"source": {"file:" + logFile}, "follow": {"1"}}.Encode())
	if !strings.Contains(body, `http-equiv="refresh" content="5"`) || !strings.Contains(body, "stop following") {
		t.Error("follow should refresh the page")
	}
}

func TestTuningPage(t *testing.T) {
	cfg := validConfig()
	cfg.Monitor.Enabled = true
	h := newHarness(t, cfg, true)
	h.ui.Monitor = fakeMonitor(t)
	etc := t.TempDir()
	h.ui.PHPEtc, h.ui.WebBackupDir = etc, t.TempDir()
	os.MkdirAll(filepath.Join(etc, "8.3", "fpm", "pool.d"), 0o755)
	poolFile := filepath.Join(etc, "8.3", "fpm", "pool.d", "www.conf")
	os.WriteFile(poolFile, []byte("[www]\nlisten = /run/php/php8.3-fpm.sock\npm = static\npm.max_children = 5\npm.max_requests = 500\npm.status_path = /liops-fpm-status\n"), 0o644)

	_, _, body := h.get("/monitoring/tuning")
	for _, want := range []string{`href="/monitoring/tuning">Tuning`, "pm.max_children", "5 → ", "Apply", "never touched"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Tuning page should contain %q", want)
		}
	}
	code, loc, body := h.post("/monitoring/tuning", "/monitoring/tuning/apply", url.Values{"id": {"pool/8.3/www/pm.max_children"}})
	if code != 303 || loc != "/monitoring/tuning" {
		t.Fatalf("apply: %d %q\n%s", code, loc, body)
	}
	data, _ := os.ReadFile(poolFile)
	if strings.Contains(string(data), "pm.max_children = 5\n") || !strings.Contains(string(data), "; liops tuning") {
		t.Errorf("pool after apply:\n%s", data)
	}
	_, _, body = h.get("/monitoring/tuning")
	if !strings.Contains(body, "Applied:") || !strings.Contains(body, "Revert") || !strings.Contains(body, "was: pm.max_children 5") {
		t.Errorf("after apply:\n%s", body)
	}
	if code, _, _ := h.post("/monitoring/tuning", "/monitoring/tuning/revert", url.Values{"index": {"0"}}); code != 303 {
		t.Fatalf("revert: %d", code)
	}
	if data, _ := os.ReadFile(poolFile); !strings.Contains(string(data), "pm.max_children = 5 ; liops tuning") {
		t.Errorf("pool after revert:\n%s", data)
	}
	entries, _ := h.st.AuditLog(0, 10)
	if len(entries) < 2 || entries[0].Action != "tuning" || !strings.HasPrefix(entries[0].Detail, "revert") {
		t.Errorf("audit = %+v", entries)
	}
}

func TestSetupRestartNeedsPending(t *testing.T) {
	h := newHarness(t, validConfig(), false)
	h.ui.setup.mu.Lock()
	h.ui.setup.task = &applyTask{Running: false}
	h.ui.setup.mu.Unlock()
	code, loc, _ := h.post("/setup/apply/progress", "/setup/apply/restart", url.Values{"restart": {"now"}})
	if code != 303 || loc != "/setup/apply/progress" {
		t.Fatalf("restart: %d %q", code, loc)
	}
	if task := h.ui.setup.snapshot(); task.Running {
		t.Error("must not restart PostgreSQL when no restart is pending")
	}
}

func TestMonitorRestartSkippedWhenNotPending(t *testing.T) {
	old := fakePendingRestart
	fakePendingRestart = "f"
	t.Cleanup(func() { fakePendingRestart = old })
	h := newHarness(t, validConfig(), true)
	code, loc, _ := h.post("/settings?tab=monitoring", "/settings/monitor/restart", url.Values{})
	if code != 303 || !strings.HasPrefix(loc, "/settings") {
		t.Fatalf("restart: %d %q", code, loc)
	}
	_, _, body := h.get("/settings?tab=monitoring")
	if !strings.Contains(body, "need a restart right now") {
		t.Errorf("should say no restart was needed:\n%s", body)
	}
}

func TestOpenLogFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(secret, []byte("storage-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "steal.log")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openLogFile(link); err == nil {
		f.Close()
		t.Fatal("a symlink must not be opened")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("got %v", err)
	}
	f, err := openLogFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}
