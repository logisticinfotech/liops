// SPDX-License-Identifier: Apache-2.0

package pgbackrest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/pgtijori/app/engines/postgres"
	"github.com/logisticinfotech/pgtijori/config"
)

func testConfig() *config.Config {
	c := config.Default()
	c.PgBackRest.Stanza = "main"
	c.PgBackRest.ArchiveTimeout = config.Duration(15 * time.Minute)
	c.Storage.S3 = config.S3{
		Endpoint: "example-account.r2.cloudflarestorage.com", Region: "auto", Bucket: "example-bucket",
		AccessKeyID: "key", SecretAccessKey: "secret", PathStyle: true, VerifyTLS: true,
	}
	return c
}

func TestRenderConfig(t *testing.T) {
	conf, err := RenderConfig(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[global]\n",
		"repo1-path=/main/pgbackrest\n", // the server's folder in the bucket
		"repo1-s3-bucket=example-bucket\n",
		"repo1-s3-key-secret=secret\n",
		"repo1-s3-uri-style=path\n",
		"repo1-retention-full-type=time\n",
		"repo1-retention-full=30\n",
		"repo1-bundle=y\n",
		"process-max=4\n",
		"[main]\npg1-path=/var/lib/postgresql/18/main\npg1-port=5432\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config is missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "verify-tls") || strings.Contains(conf, "storage-port") {
		t.Error("verify-tls and storage-port should only be written when needed")
	}
}

func TestRenderConfigSplitsPort(t *testing.T) {
	c := testConfig()
	c.Storage.S3.Endpoint = "minio:9000"
	conf, err := RenderConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(conf, "repo1-s3-endpoint=minio\n") || !strings.Contains(conf, "repo1-storage-port=9000\n") {
		t.Errorf("port should be split from the endpoint:\n%s", conf)
	}
}

func TestRenderConfigRejectsLineBreaks(t *testing.T) {
	c := testConfig()
	c.Storage.S3.SecretAccessKey = "secret\nrepo1-path=/elsewhere"
	if _, err := RenderConfig(c); err == nil {
		t.Fatal("a line break in a value must be rejected")
	}
}

func TestRenderPostgresConf(t *testing.T) {
	got := RenderPostgresConf(testConfig())
	for _, want := range []string{
		"archive_mode = on\n",
		"archive_command = 'pgbackrest --stanza=main archive-push %p'\n",
		"archive_timeout = 900\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

const infoJSON = `[{
  "archive": [{"database": {"id": 1, "repo-key": 1}, "id": "18-1",
               "max": "000000010000000000000009", "min": "000000010000000000000002"}],
  "backup": [
    {"label": "20261005-013001F_20261006-013002D", "type": "diff", "error": false,
     "timestamp": {"start": 1791250202, "stop": 1791250238},
     "info": {"size": 52428800, "delta": 1048576, "repository": {"size": 9437184, "delta": 262144}}},
    {"label": "20261005-013001F", "type": "full", "error": false,
     "timestamp": {"start": 1791163801, "stop": 1791163837},
     "info": {"size": 52428800, "delta": 52428800, "repository": {"size": 9437184, "delta": 9437184}}}
  ],
  "cipher": "none", "name": "main",
  "status": {"code": 0, "lock": {"backup": {"held": false}}, "message": "ok"}
}, {
  "archive": [], "backup": [], "name": "other",
  "status": {"code": 2, "message": "no valid backups"}
}]`

func TestParseInfo(t *testing.T) {
	info, err := ParseInfo([]byte(infoJSON), "main")
	if err != nil {
		t.Fatal(err)
	}
	if !info.OK || info.Status != "ok" {
		t.Errorf("status = %q ok=%v", info.Status, info.OK)
	}
	if len(info.Backups) != 2 || info.Backups[0].Type != "full" {
		t.Fatalf("backups should be sorted oldest first: %+v", info.Backups)
	}
	if got := info.RestoreFrom(); !got.Equal(time.Unix(1791163837, 0)) {
		t.Errorf("RestoreFrom = %v", got)
	}
	last := info.Last("")
	if last.Type != "diff" || last.Duration() != 36*time.Second || last.RepoSize != 262144 {
		t.Errorf("Last = %+v", last)
	}
	if info.Last("full").Label != "20261005-013001F" {
		t.Error("Last(full) picked the wrong backup")
	}
	if info.WALMax != "000000010000000000000009" {
		t.Errorf("WALMax = %q", info.WALMax)
	}

	other, err := ParseInfo([]byte(infoJSON), "other")
	if err != nil {
		t.Fatal(err)
	}
	if other.OK || other.Last("") != nil || !other.RestoreFrom().IsZero() {
		t.Errorf("empty stanza parsed wrongly: %+v", other)
	}

	if info.Running || other.Running {
		t.Error("no backup is running in the sample")
	}
	if info.SystemID != "" {
		t.Errorf("SystemID = %q, the sample has no db list", info.SystemID)
	}
	running, err := ParseInfo([]byte(`[{"name":"main","archive":[],"backup":[],
		"db":[{"id":1,"system-id":7000000000000000001,"version":"17"},{"id":2,"system-id":7683116411252273504,"version":"18"}],
		"status":{"code":2,"message":"no valid backups",
		"lock":{"backup":{"held":true,"size":134772515,"size-cplt":119095330}}}}]`), "main")
	if err != nil || !running.Running || running.Size != 134772515 || running.SizeDone != 119095330 ||
		running.SystemID != "7683116411252273504" {
		t.Errorf("running backup: %+v, %v", running, err)
	}

	if _, err := ParseInfo([]byte(infoJSON), "missing"); err == nil {
		t.Error("an unknown stanza should be an error")
	}
}

func TestEnsureDirFixesATightDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pgbackrest")
	if err := os.Mkdir(dir, 0o700); err != nil { // as a package might leave it
		t.Fatal(err)
	}
	if err := ensureDir(dir, 0o750, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o750 {
		t.Errorf("mode = %o, want 750", info.Mode().Perm())
	}
}

func TestWriteIfChangedChecksModeAndOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pgbackrest.conf")
	uid, gid := os.Getuid(), os.Getgid()
	if changed, err := writeIfChanged(path, "a", 0o640, uid, gid); err != nil || !changed {
		t.Fatalf("first write: %v %v", changed, err)
	}
	if changed, _ := writeIfChanged(path, "a", 0o640, uid, gid); changed {
		t.Error("same content, mode and owner: nothing to do")
	}
	os.Chmod(path, 0o600) // right content, wrong mode
	if changed, _ := writeIfChanged(path, "a", 0o640, uid, gid); !changed {
		t.Error("a wrong mode must be fixed even when the content is right")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	if ok, _ := upToDate(path, "a", 0o640, uid+1, gid); ok {
		t.Error("a wrong owner must count as out of date")
	}
}

func homebrewConfig() *config.Config {
	c := testConfig()
	c.Postgres.Layout = config.LayoutHomebrew
	c.Postgres.DataDir = postgres.HomebrewPrefix + "/var/postgresql@18"
	return c
}

func TestHomebrewLayout(t *testing.T) {
	c := homebrewConfig()
	prefix := postgres.HomebrewPrefix
	if got := ConfPath(c); got != prefix+"/etc/pgbackrest/pgbackrest.conf" {
		t.Errorf("ConfPath = %q", got)
	}
	want := prefix + "/bin/pgbackrest --config=" + prefix + "/etc/pgbackrest/pgbackrest.conf --stanza=main archive-push %p"
	if got := ArchiveCommand(c); got != want {
		t.Errorf("ArchiveCommand = %q, want %q", got, want)
	}
	conf, err := RenderConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"log-path=" + prefix + "/var/log/pgbackrest\n", "pg1-socket-path=/tmp\n"} {
		if !strings.Contains(conf, want) {
			t.Errorf("missing %q in:\n%s", want, conf)
		}
	}
	e := New(c, nil)
	if cmd := e.cmd("check"); cmd.Name != prefix+"/bin/pgbackrest" || cmd.Args[0] != "--config="+ConfPath(c) || cmd.Args[1] != "--stanza=main" {
		t.Errorf("cmd = %+v", cmd)
	}

	// Debian stays as it was: pgBackRest's default config, found on PATH.
	if got := ArchiveCommand(testConfig()); got != "pgbackrest --stanza=main archive-push %p" {
		t.Errorf("debian ArchiveCommand = %q", got)
	}
	if conf, _ := RenderConfig(testConfig()); strings.Contains(conf, "log-path") || strings.Contains(conf, "socket") {
		t.Errorf("debian config shouldn't set log-path or the socket:\n%s", conf)
	}
}

func TestIncludeConfD(t *testing.T) {
	path := filepath.Join(t.TempDir(), "postgresql.conf")
	os.WriteFile(path, []byte("port = 5432\n#include_dir = 'conf.d'"), 0o600)
	if changed, err := includeConfD(path); err != nil || !changed {
		t.Fatalf("first run: %v %v", changed, err)
	}
	if changed, err := includeConfD(path); err != nil || changed {
		t.Fatalf("second run must find the line: %v %v", changed, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(data), "port = 5432\n#include_dir = 'conf.d'\n\n# Added by pgtijori") ||
		!strings.HasSuffix(string(data), "\ninclude_dir = 'conf.d'\n") {
		t.Errorf("postgresql.conf =\n%s", data)
	}
}
