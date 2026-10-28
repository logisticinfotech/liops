// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
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

// fakeServer answers like a PostgreSQL 18 server with pgBackRest and one database.
func fakeServer(program string, c command.Cmd) ([]byte, error) {
	args := strings.Join(c.Args, " ")
	now := time.Now().Unix()
	switch program {
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

	h.ui, err = New(h.svc)
	if err != nil {
		t.Fatal(err)
	}
	h.ui.Cmd = fake
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
