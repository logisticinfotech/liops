// SPDX-License-Identifier: Apache-2.0

package pgbackrest

import (
	"strings"
	"testing"
	"time"

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
		"repo1-path=/pgbackrest\n",
		"repo1-s3-bucket=example-bucket\n",
		"repo1-s3-key-secret=secret\n",
		"repo1-s3-uri-style=path\n",
		"repo1-retention-full-type=time\n",
		"repo1-retention-full=30\n",
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

	if _, err := ParseInfo([]byte(infoJSON), "missing"); err == nil {
		t.Error("an unknown stanza should be an error")
	}
}
