// SPDX-License-Identifier: Apache-2.0

// Package config defines liops's configuration file, its defaults, and how it is
// loaded, validated and saved.
//
// The file holds secrets (storage keys, SMTP password), so Load refuses a file that
// group or others can read, and Save always writes it with mode 0600.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// DefaultPath is where liops looks for its config when --config is not given.
const DefaultPath = "/etc/liops/config.yaml"

// Config is the whole configuration file.
type Config struct {
	// Engine is the database this server runs: EnginePostgres or EngineMySQL. Only that
	// engine's sections (postgres + pgbackrest, or mysql) are used and validated.
	Engine     string     `yaml:"engine"`
	Server     Server     `yaml:"server"`
	UI         UI         `yaml:"ui"`
	MySQL      MySQL      `yaml:"mysql"`
	Postgres   Postgres   `yaml:"postgres"`
	PgBackRest PgBackRest `yaml:"pgbackrest"`
	Dumps      Dumps      `yaml:"dumps"`
	Storage    Storage    `yaml:"storage"`
	Email      Email      `yaml:"email"`
	Report     Report     `yaml:"report"`
	Monitor    Monitor    `yaml:"monitor"`
}

// Monitor is the per-server monitoring (docs/PLAN-monitor.md), run by the separate,
// unprivileged liops-monitor service.
type Monitor struct {
	// Enabled starts the liops-monitor service. Off until turned on in Settings.
	Enabled bool `yaml:"enabled"`
}

// Server describes this machine.
type Server struct {
	// Name is shown in emails and the UI. Empty means the machine's hostname.
	Name string `yaml:"name"`
	// Timezone that all schedules are written in, e.g. "Asia/Kolkata". The server
	// clock itself can stay on UTC.
	Timezone string `yaml:"timezone"`
}

// UI is the web interface. It only ever listens on a loopback address and is reached
// through an SSH tunnel.
type UI struct {
	Listen string `yaml:"listen"`
}

// Database engines.
const (
	EnginePostgres = "postgres"
	EngineMySQL    = "mysql"
)

// MySQL identifies the local MySQL server to back up (engine: mysql).
type MySQL struct {
	// BackupName is the server's folder in the bucket, like pgbackrest.stanza for PostgreSQL.
	BackupName string `yaml:"backup_name"`
	Socket     string `yaml:"socket"`
	// DefaultsFile holds the login of liops's own MySQL user. `liops setup` writes it
	// (root only, mode 0600), so the password never appears on a command line.
	DefaultsFile string `yaml:"defaults_file"`
	DataDir      string `yaml:"data_dir"`
	// BinDir holds mysql, mysqldump, mysqlbinlog and xtrabackup. Empty means /usr/bin.
	BinDir string `yaml:"bin_dir"`
	// ConfFile is where setup writes the server settings point-in-time backups need
	// (GTIDs, binlog expiry), so they survive a restart and a restore. MySQL must read it
	// at startup.
	ConfFile string `yaml:"conf_file"`
	// FullSchedule and DiffSchedule are the nightly backup times. Until point-in-time
	// backups for MySQL are built (docs/PLAN-mysql.md, M2), the dumps run at these times.
	FullSchedule string `yaml:"full_schedule"`
	DiffSchedule string `yaml:"diff_schedule"`
	// RetentionDays is how far back a point-in-time restore can go.
	RetentionDays int `yaml:"retention_days"`
	// BinlogInterval is how often binlogs are shipped to storage: the most a total loss
	// of the server can lose.
	BinlogInterval Duration `yaml:"binlog_interval"`
}

// Postgres layouts: where a PostgreSQL install keeps its programs and settings, and
// how it is restarted.
const (
	// LayoutDebian is PostgreSQL from PGDG on Debian/Ubuntu (pg_lsclusters, systemd).
	// It is what liops is built for.
	LayoutDebian = "debian"
	// LayoutHomebrew is Homebrew's postgresql@N on macOS, for development.
	LayoutHomebrew = "homebrew"
)

// Postgres identifies the local cluster to back up.
type Postgres struct {
	// Layout is LayoutDebian or LayoutHomebrew. The setup wizard sets it.
	Layout  string `yaml:"layout"`
	Version int    `yaml:"version"`
	Cluster string `yaml:"cluster"`
	DataDir string `yaml:"data_dir"`
	Port    int    `yaml:"port"`
	OSUser  string `yaml:"os_user"`
	// BinDir holds psql, pg_dump and pg_dumpall. Empty means the layout's location
	// for Version: /usr/lib/postgresql/<version>/bin, or <brew prefix>/opt/postgresql@<version>/bin.
	BinDir string `yaml:"bin_dir"`
}

// PgBackRest is layer 1: point-in-time backups (full + differential + WAL).
type PgBackRest struct {
	Stanza         string   `yaml:"stanza"`
	ArchiveTimeout Duration `yaml:"archive_timeout"`
	RetentionDays  int      `yaml:"retention_days"`
	FullSchedule   string   `yaml:"full_schedule"`
	DiffSchedule   string   `yaml:"diff_schedule"`
}

// Dumps is layer 2: a nightly pg_dump of every database, run after the nightly backup.
type Dumps struct {
	Enabled              bool       `yaml:"enabled"`
	RetentionDays        int        `yaml:"retention_days"`
	DeletedRetentionDays int        `yaml:"deleted_retention_days"`
	Concurrency          int        `yaml:"concurrency"`
	Exclude              []string   `yaml:"exclude"`
	Labels               LabelQuery `yaml:"labels"`
}

// LabelQuery optionally maps database names to friendly labels (e.g. tenant subdomains).
// liops builds SELECT db_column, label_column FROM table itself: leftover sql in a
// config from before this is ignored and never run.
type LabelQuery struct {
	Database    string `yaml:"database"`
	Table       string `yaml:"table"`
	DBColumn    string `yaml:"db_column"`
	LabelColumn string `yaml:"label_column"`
	SQL         string `yaml:"sql,omitempty"` // leftover free SQL: never executed
}

// labelIdentRe is one identifier: a schema, table or column name.
var labelIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// QuoteIdent quotes a table or column name for PostgreSQL (") or MySQL (backticks).
// Table may be schema.table (one optional dot); each part is quoted separately.
func QuoteIdent(name string, mysql bool) (string, error) {
	parts := strings.Split(name, ".")
	if len(parts) == 0 || len(parts) > 2 {
		return "", fmt.Errorf("%q isn't a table or column name (letters, digits and underscores, starting with a letter or underscore; one optional schema.)", name)
	}
	quoted := make([]string, 0, len(parts))
	for _, p := range parts {
		if !labelIdentRe.MatchString(p) {
			return "", fmt.Errorf("%q isn't a table or column name (letters, digits and underscores, starting with a letter or underscore; one optional schema.)", name)
		}
		if mysql {
			quoted = append(quoted, "`"+p+"`")
		} else {
			quoted = append(quoted, `"`+p+`"`)
		}
	}
	return strings.Join(quoted, "."), nil
}

// Configured reports whether the four identifier fields are set, so a query can be built.
func (q LabelQuery) Configured() bool {
	return q.Database != "" && q.Table != "" && q.DBColumn != "" && q.LabelColumn != ""
}

// SelectSQL is the quoted SELECT liops runs. Leftover q.SQL is never used.
func (q LabelQuery) SelectSQL(mysql bool) (string, error) {
	table, err := QuoteIdent(q.Table, mysql)
	if err != nil {
		return "", err
	}
	dbCol, err := QuoteIdent(q.DBColumn, mysql)
	if err != nil {
		return "", err
	}
	labelCol, err := QuoteIdent(q.LabelColumn, mysql)
	if err != nil {
		return "", err
	}
	return "SELECT " + dbCol + ", " + labelCol + " FROM " + table, nil
}

// Storage is where both layers are stored.
type Storage struct {
	Type string `yaml:"type"` // only "s3" in v1 (R2, AWS S3, B2, Wasabi, MinIO)
	S3   S3     `yaml:"s3"`
}

// S3 configures an S3-compatible bucket.
type S3 struct {
	Endpoint        string `yaml:"endpoint"` // host only, e.g. <account>.r2.cloudflarestorage.com
	Region          string `yaml:"region"`
	Bucket          string `yaml:"bucket"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	PathStyle       bool   `yaml:"path_style"`
	// VerifyTLS can only be turned off for test setups with self-signed certificates.
	VerifyTLS bool `yaml:"verify_tls"`
}

// Email is the SMTP account used for the daily summary and failure alerts.
type Email struct {
	SMTPHost string `yaml:"smtp_host"`
	SMTPPort int    `yaml:"smtp_port"`
	// TLS is "starttls" (usually port 587) or "tls" (usually port 465). There is no
	// plaintext option: the SMTP password must never cross the network unencrypted.
	TLS      string   `yaml:"tls"`
	Username string   `yaml:"username"`
	Password string   `yaml:"password"`
	From     string   `yaml:"from"`
	To       []string `yaml:"to"`
}

// Report is the daily summary email.
type Report struct {
	Schedule string `yaml:"schedule"`
}

// Default returns a configuration with every non-secret setting filled in.
// Storage and email still have to be completed before liops can run.
func Default() *Config {
	return &Config{
		Engine: EnginePostgres,
		Server: Server{Timezone: "Asia/Kolkata"},
		UI:     UI{Listen: "127.0.0.1:8899"},
		MySQL: MySQL{
			Socket:         "/var/run/mysqld/mysqld.sock",
			DefaultsFile:   "/etc/liops/mysql.cnf",
			DataDir:        "/var/lib/mysql",
			ConfFile:       "/etc/mysql/mysql.conf.d/zz-liops.cnf",
			FullSchedule:   "30 1 * * 0",   // Sunday 01:30
			DiffSchedule:   "30 1 * * 1-6", // Monday-Saturday 01:30
			RetentionDays:  30,
			BinlogInterval: Duration(5 * time.Minute),
		},
		Postgres: Postgres{
			Layout:  LayoutDebian,
			Version: 18,
			Cluster: "main",
			DataDir: "/var/lib/postgresql/18/main",
			Port:    5432,
			OSUser:  "postgres",
		},
		PgBackRest: PgBackRest{
			ArchiveTimeout: Duration(time.Hour),
			RetentionDays:  30,
			FullSchedule:   "30 1 * * 0",   // Sunday 01:30
			DiffSchedule:   "30 1 * * 1-6", // Monday-Saturday 01:30
		},
		Dumps: Dumps{
			Enabled:              true,
			RetentionDays:        7,
			DeletedRetentionDays: 14,
			Concurrency:          1,
			Exclude:              []string{"postgres"},
		},
		Storage: Storage{
			Type: "s3",
			S3:   S3{Region: "auto", PathStyle: true, VerifyTLS: true},
		},
		Email: Email{
			SMTPHost: "smtp.gmail.com",
			SMTPPort: 587,
			TLS:      "starttls",
		},
		Report: Report{Schedule: "30 7 * * *"}, // 07:30
	}
}

// Load reads and validates the config file at path.
func Load(path string) (*Config, error) {
	cfg, err := LoadDraft(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// LoadDraft reads the config file without validating it, for the setup wizard, which
// fills it in step by step. A missing file gives the defaults.
func LoadDraft(path string) (*Config, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return nil, err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %04o; it holds secrets, so it must not be readable by group or others (run: chmod 600 %s)", path, perm, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes YAML on top of the defaults and validates the result.
func Parse(data []byte) (*Config, error) {
	cfg, err := Decode(data)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Decode decodes YAML on top of the defaults without validating.
// Unknown keys are an error, so a typo can't silently fall back to a default.
func Decode(data []byte) (*Config, error) {
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return cfg, nil
}

// Clone returns a deep copy, so a draft can be edited without touching the live config.
func (c *Config) Clone() *Config {
	data, err := yaml.Marshal(c)
	if err != nil {
		panic(err) // a Config always marshals
	}
	clone, err := Decode(data)
	if err != nil {
		panic(err)
	}
	return clone
}

// Save writes the config atomically with mode 0600: a temporary file in the same
// directory, fsync, then rename over the old file.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Location returns the schedule timezone.
func (c *Config) Location() (*time.Location, error) {
	return time.LoadLocation(c.Server.Timezone)
}

// BackupName is the server's backup name: its folder in the bucket. For PostgreSQL it
// is also the pgBackRest stanza.
func (c *Config) BackupName() string {
	if c.Engine == EngineMySQL {
		return c.MySQL.BackupName
	}
	return c.PgBackRest.Stanza
}

// IsMySQL reports whether this server runs MySQL rather than PostgreSQL.
func (c *Config) IsMySQL() bool { return c.Engine == EngineMySQL }

// backupNameRe is what a backup name (pgbackrest.stanza, mysql.backup_name) may look
// like. It is the server's folder in the bucket, and for PostgreSQL the pgBackRest stanza.
var backupNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// identityNameRe is a PostgreSQL cluster name or OS user: interpolated into paths and
// commands, so it may only be a single path element of letters, digits, '-' or '_'.
var identityNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

// Validate reports every problem at once, joined with errors.Join.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if _, err := c.Location(); err != nil {
		add("server.timezone: %v", err)
	}

	if err := validateLoopback(c.UI.Listen); err != nil {
		add("ui.listen: %v", err)
	}

	switch c.Engine {
	case EnginePostgres:
		c.validatePostgres(add, &errs)
	case EngineMySQL:
		c.validateMySQL(add, &errs)
	default:
		add("engine: %q must be %q or %q", c.Engine, EnginePostgres, EngineMySQL)
	}

	if c.Dumps.Enabled {
		if c.Dumps.RetentionDays < 1 {
			add("dumps.retention_days: must be at least 1")
		}
		if c.Dumps.DeletedRetentionDays < 1 {
			add("dumps.deleted_retention_days: must be at least 1")
		}
		if c.Dumps.Concurrency < 1 || c.Dumps.Concurrency > 8 {
			add("dumps.concurrency: %d must be between 1 and 8", c.Dumps.Concurrency)
		}
		q := c.Dumps.Labels
		partial := q.Table != "" || q.DBColumn != "" || q.LabelColumn != ""
		if partial && !q.Configured() {
			add("dumps.labels: set database, table, db_column and label_column, or none of them")
		}
		if q.Configured() {
			for _, pair := range []struct{ field, name string }{
				{"table", q.Table}, {"db_column", q.DBColumn}, {"label_column", q.LabelColumn},
			} {
				if _, err := QuoteIdent(pair.name, false); err != nil {
					add("dumps.labels.%s: %s", pair.field, err.Error())
				}
			}
		}
	}

	switch c.Storage.Type {
	case "s3":
		s := c.Storage.S3
		if s.Endpoint == "" {
			add("storage.s3.endpoint: required (e.g. <account-id>.r2.cloudflarestorage.com)")
		} else if strings.Contains(s.Endpoint, "://") || strings.Contains(s.Endpoint, "/") {
			add("storage.s3.endpoint: give the host only, without https:// or a path")
		}
		if s.Region == "" {
			add("storage.s3.region: required (\"auto\" for Cloudflare R2)")
		}
		if s.Bucket == "" {
			add("storage.s3.bucket: required")
		}
		if s.AccessKeyID == "" || s.SecretAccessKey == "" {
			add("storage.s3: access_key_id and secret_access_key are required")
		}
	default:
		add("storage.type: %q is not supported (supported: s3)", c.Storage.Type)
	}

	if c.Email.SMTPHost == "" {
		add("email.smtp_host: required")
	}
	if c.Email.SMTPPort < 1 || c.Email.SMTPPort > 65535 {
		add("email.smtp_port: %d is not a valid port", c.Email.SMTPPort)
	}
	if c.Email.TLS != "starttls" && c.Email.TLS != "tls" {
		add("email.tls: %q must be starttls (port 587) or tls (port 465)", c.Email.TLS)
	}
	if c.Email.Username == "" || c.Email.Password == "" {
		add("email: username and password are required (for Gmail: the address and an app password)")
	}
	if _, err := mail.ParseAddress(c.Email.From); err != nil {
		add("email.from: %q is not an email address", c.Email.From)
	}
	if len(c.Email.To) == 0 {
		add("email.to: at least one recipient is required")
	}
	for _, to := range c.Email.To {
		if _, err := mail.ParseAddress(to); err != nil {
			add("email.to: %q is not an email address", to)
		}
	}

	checkSchedule(&errs, "report.schedule", c.Report.Schedule)

	return errors.Join(errs...)
}

// CheckUIListen checks ui.listen on its own, for a draft config that isn't complete yet.
func CheckUIListen(listen string) error { return validateLoopback(listen) }

// validateLoopback makes sure the UI can only be reached from the machine itself.
func validateLoopback(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("%q: %v", listen, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("%q: invalid port", listen)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%q: the UI may only listen on a loopback address such as 127.0.0.1 (reach it through an SSH tunnel)", listen)
	}
	return nil
}

var scheduleParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// validatePostgres checks the postgres and pgbackrest sections.
func (c *Config) validatePostgres(add func(string, ...any), errs *[]error) {
	if c.Postgres.Layout != LayoutDebian && c.Postgres.Layout != LayoutHomebrew {
		add("postgres.layout: %q must be %q or %q", c.Postgres.Layout, LayoutDebian, LayoutHomebrew)
	}
	if c.Postgres.Version < 13 {
		add("postgres.version: %d is not supported (need 13 or newer)", c.Postgres.Version)
	}
	if c.Postgres.Cluster == "" {
		add("postgres.cluster: required")
	} else if !identityNameRe.MatchString(c.Postgres.Cluster) {
		add("postgres.cluster: %q isn't a valid cluster name: use letters, digits, '-' or '_', starting with a letter or digit", c.Postgres.Cluster)
	}
	if !filepath.IsAbs(c.Postgres.DataDir) {
		add("postgres.data_dir: must be an absolute path")
	}
	if c.Postgres.Port < 1 || c.Postgres.Port > 65535 {
		add("postgres.port: %d is not a valid port", c.Postgres.Port)
	}
	if c.Postgres.OSUser == "" {
		add("postgres.os_user: required")
	} else if !identityNameRe.MatchString(c.Postgres.OSUser) {
		add("postgres.os_user: %q isn't a valid user name: use letters, digits, '-' or '_', starting with a letter or digit", c.Postgres.OSUser)
	}
	if c.Postgres.BinDir != "" && !filepath.IsAbs(c.Postgres.BinDir) {
		add("postgres.bin_dir: must be an absolute path (or empty for the layout's default)")
	}

	if c.PgBackRest.Stanza == "" {
		add("pgbackrest.stanza: required: a backup name for this server, e.g. app-prod-1")
	} else if !backupNameRe.MatchString(c.PgBackRest.Stanza) {
		add("pgbackrest.stanza: %q can't be used as a backup name: use 1-63 letters, digits, '-', '_' or '.', starting with a letter or digit", c.PgBackRest.Stanza)
	}
	if t := time.Duration(c.PgBackRest.ArchiveTimeout); t < time.Minute || t > 24*time.Hour {
		add("pgbackrest.archive_timeout: %s must be between 1m and 24h", t)
	}
	if c.PgBackRest.RetentionDays < 1 {
		add("pgbackrest.retention_days: must be at least 1")
	}
	checkSchedule(errs, "pgbackrest.full_schedule", c.PgBackRest.FullSchedule)
	checkSchedule(errs, "pgbackrest.diff_schedule", c.PgBackRest.DiffSchedule)
}

// validateMySQL checks the mysql section.
func (c *Config) validateMySQL(add func(string, ...any), errs *[]error) {
	m := c.MySQL
	if m.BackupName == "" {
		add("mysql.backup_name: required: a backup name for this server, e.g. app-prod-1")
	} else if !backupNameRe.MatchString(m.BackupName) {
		add("mysql.backup_name: %q can't be used as a backup name: use 1-63 letters, digits, '-', '_' or '.', starting with a letter or digit", m.BackupName)
	}
	if !filepath.IsAbs(m.Socket) {
		add("mysql.socket: must be an absolute path, e.g. /var/run/mysqld/mysqld.sock")
	}
	if !filepath.IsAbs(m.DefaultsFile) {
		add("mysql.defaults_file: must be an absolute path")
	}
	if !filepath.IsAbs(m.DataDir) {
		add("mysql.data_dir: must be an absolute path")
	}
	if m.BinDir != "" && !filepath.IsAbs(m.BinDir) {
		add("mysql.bin_dir: must be an absolute path (or empty for /usr/bin)")
	}
	if !filepath.IsAbs(m.ConfFile) {
		add("mysql.conf_file: must be an absolute path, e.g. /etc/mysql/mysql.conf.d/zz-liops.cnf")
	}
	if m.RetentionDays < 1 {
		add("mysql.retention_days: must be at least 1")
	}
	if t := time.Duration(m.BinlogInterval); t < time.Minute || t > time.Hour {
		add("mysql.binlog_interval: %s must be between 1m and 1h", t)
	}
	checkSchedule(errs, "mysql.full_schedule", m.FullSchedule)
	checkSchedule(errs, "mysql.diff_schedule", m.DiffSchedule)
}

func checkSchedule(errs *[]error, field, spec string) {
	if _, err := scheduleParser.Parse(spec); err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a valid 5-field cron schedule: %v", field, spec, err))
	}
}

// Duration is a time.Duration written in YAML as "1h", "15m", etc.
type Duration time.Duration

// String formats d without zero units: "1h" not "1h0m0s".
func (d Duration) String() string {
	td := time.Duration(d)
	if td == 0 {
		return "0s"
	}
	if td < 0 {
		return "-" + Duration(-td).String()
	}
	var b strings.Builder
	if h := td / time.Hour; h > 0 {
		fmt.Fprintf(&b, "%dh", h)
		td -= h * time.Hour
	}
	if m := td / time.Minute; m > 0 {
		fmt.Fprintf(&b, "%dm", m)
		td -= m * time.Minute
	}
	if s := td / time.Second; s > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}

func (d Duration) MarshalYAML() (any, error) {
	return d.String(), nil
}

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("line %d: %q is not a duration (examples: 15m, 1h)", node.Line, node.Value)
	}
	*d = Duration(parsed)
	return nil
}
