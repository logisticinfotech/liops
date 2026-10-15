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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/logisticinfotech/pgtijori/app/auth"
	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/engines/pgbackrest"
	"github.com/logisticinfotech/pgtijori/app/engines/pgdump"
	"github.com/logisticinfotech/pgtijori/app/engines/postgres"
	"github.com/logisticinfotech/pgtijori/app/jobs"
	"github.com/logisticinfotech/pgtijori/app/notify"
	"github.com/logisticinfotech/pgtijori/app/service"
	"github.com/logisticinfotech/pgtijori/app/setup"
	"github.com/logisticinfotech/pgtijori/app/state"
	"github.com/logisticinfotech/pgtijori/app/storage"
	"github.com/logisticinfotech/pgtijori/config"
)

func TestMain(m *testing.M) {
	auth.Cost = 4 // bcrypt's minimum, so tests are fast
	os.Exit(m.Run())
}

type harness struct {
	t          *testing.T
	srv        *httptest.Server
	ui         *Server
	svc        *service.Service
	st         *state.Store
	store      *storage.Memory
	client     *http.Client
	configPath string
	tokenPath  string
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
	h := &harness{t: t, configPath: filepath.Join(dir, "config.yaml"), tokenPath: filepath.Join(dir, "setup-token"), store: storage.NewMemory()}
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
		return &jobs.Runner{
			Cfg: cfg, Server: "db-test", State: st, Store: h.store, Mail: &notify.Recorder{},
			Backrest: pgbackrest.New(cfg, fake), Dumps: pgdump.New(cfg, fake, h.store),
			Cluster: postgres.Cluster{Cfg: cfg.Postgres, Run: fake}, LockPath: filepath.Join(dir, "job.lock"),
			Now: time.Now, Log: io.Discard,
		}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.svc, err = service.New(ctx, h.configPath, filepath.Join(dir, "job.lock"), st, io.Discard, newRunner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.svc.Close)
	h.svc.WriteSettings = func(context.Context, *config.Config) (setup.Archiving, error) { return setup.Ready, nil }

	h.ui, err = New(h.svc, h.tokenPath)
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
	resp, err := h.client.PostForm(h.srv.URL+path, form)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

const testPassword = "correct-horse-battery"

// login creates a user with an enrolled authenticator and logs in.
func (h *harness) login(username string) {
	h.t.Helper()
	key, err := auth.NewEnrollment("db-test", username, "")
	if err != nil {
		h.t.Fatal(err)
	}
	hash, _ := auth.HashPassword(testPassword)
	if err := h.st.CreateUser(&state.User{Username: username, PasswordHash: hash, TOTPSecret: key.Secret, Created: time.Now()}); err != nil {
		h.t.Fatal(err)
	}
	if code, loc, _ := h.post("/login", "/login", url.Values{"username": {username}, "password": {testPassword}}); code != 303 || loc != "/login/totp" {
		h.t.Fatalf("password step: %d %s", code, loc)
	}
	otp, _ := totp.GenerateCode(key.Secret, time.Now())
	if code, loc, _ := h.post("/login/totp", "/login/totp", url.Values{"code": {otp}}); code != 303 || loc != "/" {
		h.t.Fatalf("TOTP step: %d %s", code, loc)
	}
}

func TestWizardCreatesFirstAdmin(t *testing.T) {
	h := newHarness(t, config.Default(), false)
	if _, loc, _ := h.get("/"); loc != "/login" {
		t.Fatalf("anonymous / should go to /login, got %q", loc)
	}
	if _, loc, _ := h.get("/login"); loc != "/setup" {
		t.Fatalf("with no users, /login should go to the wizard, got %q", loc)
	}
	if _, _, body := h.get("/setup"); !strings.Contains(body, "setup token") {
		t.Fatalf("the wizard starts with the token:\n%s", body)
	}

	token, err := setup.NewToken(h.tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, body := h.post("/setup", "/setup/token", url.Values{"token": {"WRONG"}}); !strings.Contains(body, "not the setup token") {
		t.Error("a wrong token must be refused")
	}
	if code, loc, _ := h.post("/setup", "/setup/token", url.Values{"token": {strings.ToLower(token)}}); code != 303 || loc != "/setup" {
		t.Fatalf("token: %d %q", code, loc)
	}
	if _, _, body := h.post("/setup", "/setup/admin", url.Values{"username": {"admin"}, "password": {"short"}, "confirm": {"short"}}); !strings.Contains(body, "at least 12") {
		t.Error("a short password must be refused")
	}
	code, loc, _ := h.post("/setup", "/setup/admin", url.Values{"username": {"admin"}, "password": {testPassword}, "confirm": {testPassword}})
	if code != 303 || loc != "/login/totp" {
		t.Fatalf("admin: %d %q", code, loc)
	}
	if _, err := os.Stat(h.tokenPath); !os.IsNotExist(err) {
		t.Error("the setup token must be deleted once the admin exists")
	}

	// Enroll the authenticator shown on the page.
	_, _, body := h.get("/login/totp")
	m := regexp.MustCompile(`<code>([A-Z2-7]+)</code>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no secret on the enrollment page:\n%s", body)
	}
	_, _, again := h.get("/login/totp")
	if !strings.Contains(again, m[1]) {
		t.Error("reloading the enrollment page must show the same secret")
	}
	otp, _ := totp.GenerateCode(m[1], time.Now())
	if code, loc, _ := h.post("/login/totp", "/login/totp", url.Values{"code": {otp}}); code != 303 || loc != "/" {
		t.Fatalf("enroll: %d %q", code, loc)
	}
	if u, _ := h.st.GetUser("admin"); !u.Enrolled() {
		t.Error("the authenticator should be saved")
	}

	// Logged in but setup isn't done: on to the first step, which lists the clusters.
	if _, loc, _ := h.get("/"); loc != "/setup" {
		t.Fatalf("before setup is complete / goes to the wizard, got %q", loc)
	}
	if _, loc, _ := h.get("/setup"); loc != "/setup/postgres" {
		t.Fatalf("got %q", loc)
	}
	if _, _, body := h.get("/setup/postgres"); !strings.Contains(body, "PostgreSQL 18 / main") {
		t.Errorf("clusters missing:\n%s", body)
	}
	code, loc, _ = h.post("/setup/postgres", "/setup/postgres", url.Values{"cluster": {"0"}, "stanza": {"main"}, "name": {"db-test"}})
	if code != 303 || loc != "/setup/storage" {
		t.Fatalf("postgres step: %d %q", code, loc)
	}
	if cfg, _ := config.LoadDraft(h.configPath); cfg.PgBackRest.Stanza != "main" || cfg.Server.Name != "db-test" {
		t.Errorf("the draft should be saved: %+v", cfg.PgBackRest)
	}
	if _, _, body := h.get("/setup/review"); !strings.Contains(body, "storage.s3.bucket") {
		t.Error("review should list what's still missing")
	}
}

func TestLoginLockout(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	hash, _ := auth.HashPassword(testPassword)
	h.st.CreateUser(&state.User{Username: "admin", PasswordHash: hash})
	for range 5 {
		if _, _, body := h.post("/login", "/login", url.Values{"username": {"admin"}, "password": {"nope"}}); !strings.Contains(body, "Wrong username or password") {
			t.Fatal("expected a wrong-password error")
		}
	}
	if _, _, body := h.post("/login", "/login", url.Values{"username": {"admin"}, "password": {testPassword}}); !strings.Contains(body, "Too many failed attempts") {
		t.Error("the right password must be refused while locked out")
	}
	log, _ := h.st.AuditLog(0, 10)
	if len(log) == 0 || log[0].OK {
		t.Errorf("failed logins should be audited: %+v", log)
	}
}

func TestDashboardAndDownload(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	h.login("admin")
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
	if code, _, _ := h.post("/databases", "/download", url.Values{"key": {"pgbackrest/archive/x"}}); code != 400 {
		t.Errorf("only dumps can be downloaded, got %d", code)
	}
	entries, _ := h.st.AuditLog(0, 20)
	found := false
	for _, e := range entries {
		if e.Action == "download" && e.User == "admin" && e.Detail == m[1] && e.OK {
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
	h.login("admin")
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
	h.login("admin")
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

	h.login("admin")
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
}

func TestEveryPageRenders(t *testing.T) {
	h := newHarness(t, validConfig(), true)
	h.login("admin")
	h.store.Add(pgdump.Key("app", time.Now()), []byte("PGDMP"), time.Now())
	h.store.Add(pgdump.GlobalsKey(time.Now()), []byte("CREATE ROLE"), time.Now())
	run := &state.Run{Job: "dumps", Start: time.Now(), Dumps: []state.DumpRun{{DB: "app", Size: 5}, {DB: "gone", Error: "boom"}}}
	h.st.StartRun(run)
	run.Status, run.End = state.StatusFailed, time.Now()
	h.st.SaveRun(run)
	h.st.SetDeleted("gone", time.Now())

	for _, path := range []string{"/", "/backups", "/databases", "/databases/app", "/databases/gone", "/jobs", "/jobs?job=dumps", fmt.Sprintf("/jobs/%d", run.ID), "/settings", "/audit", "/account"} {
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
