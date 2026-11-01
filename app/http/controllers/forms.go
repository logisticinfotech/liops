// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/engines/pgdump"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// Settings sections. Each is one form, in Settings and in the wizard.
const (
	sectionServer    = "server"
	sectionStorage   = "storage"
	sectionEmail     = "email"
	sectionSchedules = "schedules"
	sectionDumps     = "dumps"
	sectionLabels    = "labels"
	sectionMonitor   = "monitor"
)

// applyForm copies one section's form values into cfg. Secret fields left empty keep
// their current value, so the page never has to send secrets back to the browser.
func applyForm(cfg *config.Config, section string, f url.Values) error {
	var errs []error
	atoi := func(field string, dst *int) {
		v := strings.TrimSpace(f.Get(field))
		n, err := strconv.Atoi(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not a number", field, v))
			return
		}
		*dst = n
	}
	text := func(field string) string { return strings.TrimSpace(f.Get(field)) }
	checked := func(field string) bool { return f.Get(field) == "on" }

	switch section {
	case sectionServer:
		cfg.Server.Name = text("name")
		cfg.Server.Timezone = text("timezone")
	case sectionStorage:
		s := &cfg.Storage.S3
		s.Endpoint, s.Region, s.Bucket = text("endpoint"), text("region"), text("bucket")
		s.AccessKeyID = text("access_key_id")
		if v := text("secret_access_key"); v != "" {
			s.SecretAccessKey = v
		}
		s.PathStyle, s.VerifyTLS = checked("path_style"), checked("verify_tls")
	case sectionEmail:
		e := &cfg.Email
		e.SMTPHost, e.TLS, e.Username, e.From = text("smtp_host"), text("tls"), text("username"), text("from")
		atoi("smtp_port", &e.SMTPPort)
		if v := f.Get("password"); v != "" {
			e.Password = v
		}
		e.To = splitList(f.Get("to"))
	case sectionSchedules:
		full, diff, keep := &cfg.PgBackRest.FullSchedule, &cfg.PgBackRest.DiffSchedule, &cfg.PgBackRest.RetentionDays
		if cfg.IsMySQL() {
			full, diff, keep = &cfg.MySQL.FullSchedule, &cfg.MySQL.DiffSchedule, &cfg.MySQL.RetentionDays
		}
		readSchedule := func(prefix, fallbackField string, dest *string) {
			if clock := text(prefix + "_time"); clock != "" {
				days := text(prefix + "_days")
				if days == "" {
					days = "*"
				}
				spec, err := config.BuildCron(clock, days)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %v", prefix, err))
					return
				}
				*dest = spec
				return
			}
			if v := text(fallbackField); v != "" {
				*dest = v
			}
		}
		readSchedule("full", "full_schedule", full)
		readSchedule("diff", "diff_schedule", diff)
		readSchedule("report", "report_schedule", &cfg.Report.Schedule)
		interval, dest := "archive_timeout", &cfg.PgBackRest.ArchiveTimeout
		if cfg.IsMySQL() {
			interval, dest = "binlog_interval", &cfg.MySQL.BinlogInterval
		}
		if d, err := time.ParseDuration(text(interval)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %q is not a duration (examples: 5m, 1h)", interval, text(interval)))
		} else {
			*dest = config.Duration(d)
		}
		atoi("retention_days", keep)
	case sectionDumps:
		d := &cfg.Dumps
		d.Enabled = checked("enabled")
		atoi("dump_retention_days", &d.RetentionDays)
		atoi("deleted_retention_days", &d.DeletedRetentionDays)
		atoi("concurrency", &d.Concurrency)
		d.Exclude = splitList(f.Get("exclude"))
	case sectionMonitor:
		cfg.Monitor.Enabled = checked("monitor_enabled")
	case sectionLabels:
		cfg.Dumps.Labels.Database = text("labels_database")
		cfg.Dumps.Labels.Table = text("labels_table")
		cfg.Dumps.Labels.DBColumn = text("labels_db_column")
		cfg.Dumps.Labels.LabelColumn = text("labels_label_column")
	default:
		return fmt.Errorf("unknown settings section %q", section)
	}
	return errors.Join(errs...)
}

// splitList splits on commas and line breaks and drops empty items.
func splitList(s string) []string {
	var out []string
	for _, item := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' }) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// sectionErrors keeps the validation errors that belong to a section, so a half-filled
// wizard doesn't show errors for steps that come later.
func sectionErrors(cfg *config.Config, section string) []string {
	prefixes := map[string][]string{
		sectionServer:  {"server."},
		sectionStorage: {"storage"},
		sectionEmail:   {"email"},
		sectionSchedules: {"pgbackrest.archive_timeout", "pgbackrest.retention_days", "pgbackrest.full_schedule", "pgbackrest.diff_schedule", "report.",
			"mysql.full_schedule", "mysql.diff_schedule", "mysql.retention_days", "mysql.binlog_interval"},
		sectionDumps:   {"dumps.retention", "dumps.deleted", "dumps.concurrency"},
		sectionLabels:  {"dumps.labels"},
		sectionMonitor: {"monitor."},
	}[section]
	return filterErrors(cfg, prefixes...)
}

// filterErrors keeps validation errors starting with one of the prefixes.
func filterErrors(cfg *config.Config, prefixes ...string) []string {
	err := cfg.Validate()
	if err == nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(err.Error(), "\n") {
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}

// testResult is the outcome of a Test button.
type testResult struct {
	Section string
	OK      bool
	Message string
	Details string
}

// runTest runs a section's Test button against the (possibly unsaved) form values.
func (s *Server) runTest(ctx context.Context, cfg *config.Config, section string) testResult {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	res := testResult{Section: section}
	switch section {
	case sectionStorage:
		if errs := sectionErrors(cfg, section); len(errs) > 0 {
			res.Message = "Fill in the storage settings first."
			res.Details = strings.Join(errs, "\n")
			return res
		}
		store, err := storage.Open(cfg)
		if err == nil {
			err = store.Probe(ctx)
		}
		if err != nil {
			res.Message = "Storage test failed: " + notify.LikelyReason(err.Error()) + "."
			res.Details = err.Error()
			return res
		}
		res.OK, res.Message = true, "Storage works: liops wrote, read back and deleted a test object."
	case sectionEmail:
		if errs := sectionErrors(cfg, section); len(errs) > 0 {
			res.Message = "Fill in the email settings first."
			res.Details = strings.Join(errs, "\n")
			return res
		}
		body := "This is a test email from liops on " + s.serverName() + ".\n\nIf you're reading it, email alerts work. No action needed.\n"
		if err := (notify.SMTP{Cfg: cfg.Email}).Send(ctx, "🧪 "+s.serverName()+" — liops test email", body); err != nil {
			res.Message = "Sending failed."
			res.Details = err.Error()
			return res
		}
		res.OK, res.Message = true, "Sent a test email to "+strings.Join(cfg.Email.To, ", ")+"."
	case sectionLabels:
		if !cfg.Dumps.Labels.Configured() {
			res.Message = "There's no label query to test."
			return res
		}
		labels, err := pgdump.New(cfg, s.Cmd, nil).Labels(ctx)
		if err != nil {
			res.Message = "The label query failed."
			res.Details = err.Error()
			return res
		}
		var b strings.Builder
		n := 0
		for db, label := range labels {
			if n++; n > 50 {
				b.WriteString("…\n")
				break
			}
			fmt.Fprintf(&b, "%s → %s\n", db, label)
		}
		res.OK, res.Message, res.Details = true, fmt.Sprintf("The label query returned %d label(s).", len(labels)), b.String()
	default:
		res.Message = "This section has no test."
	}
	return res
}
