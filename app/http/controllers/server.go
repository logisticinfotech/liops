// SPDX-License-Identifier: Apache-2.0

// Package controllers serves the UI: dashboard, backups, databases and downloads,
// jobs, settings, audit log, and the setup wizard. There is no login: the UI is
// only reachable through an SSH tunnel (see PLAN.md §5.2). Pages are rendered on the
// server with html/template; the UI works without JavaScript.
package controllers

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/http/middleware"
	"github.com/logisticinfotech/pgtijori/app/notify"
	"github.com/logisticinfotech/pgtijori/app/service"
	"github.com/logisticinfotech/pgtijori/app/state"
	"github.com/logisticinfotech/pgtijori/app/version"
	"github.com/logisticinfotech/pgtijori/resources"
)

// Server is the UI.
type Server struct {
	Svc      *service.Service
	Sessions *middleware.Sessions
	Now      func() time.Time
	// Cmd runs the wizard's commands (pg_lsclusters, pgbackrest, ...); tests replace it.
	Cmd command.Runner

	pages map[string]*template.Template
	setup *wizard
}

// New parses the templates.
func New(svc *service.Service) (*Server, error) {
	s := &Server{
		Svc:      svc,
		Sessions: middleware.NewSessions(),
		Now:      time.Now,
		Cmd:      command.Exec{},
		pages:    map[string]*template.Template{},
		setup:    newWizard(),
	}
	names, err := fs.Glob(resources.FS, "views/*.html")
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if name == "views/layout.html" {
			continue
		}
		t, err := template.New("").Funcs(s.funcs()).ParseFS(resources.FS, "views/layout.html", "views/partials/*.html", name)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		s.pages[strings.TrimSuffix(strings.TrimPrefix(name, "views/"), ".html")] = t
	}
	return s, nil
}

func (s *Server) funcs() template.FuncMap {
	loc := func() *time.Location {
		if l, err := s.Svc.Config().Location(); err == nil {
			return l
		}
		return time.UTC
	}
	return template.FuncMap{
		"when": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return notify.When(t, s.Now().In(loc()))
		},
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(loc()).Format("Mon 2 Jan 2006")
		},
		"datetime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(loc()).Format("2 Jan 2006 15:04:05 MST")
		},
		"bytes":    notify.Bytes,
		"duration": notify.Duration,
		"since":    func(a, b time.Time) string { return notify.Duration(b.Sub(a)) },
		"name":     notify.Name,
		"join":     strings.Join,
		"lines":    func(s []string) string { return strings.Join(s, "\n") },
		"has":      func(m map[string]string, k string) bool { _, ok := m[k]; return ok },
	}
}

// Handler returns the UI with all protections applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(resources.FS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	s.setupRoutes(mux)

	page := func(h http.HandlerFunc) http.Handler { return s.requireSetup(h) }
	mux.Handle("GET /{$}", page(s.dashboard))
	mux.Handle("GET /backups", page(s.backups))
	mux.Handle("GET /databases", page(s.databases))
	mux.Handle("GET /databases/{db}", page(s.database))
	mux.Handle("POST /download", page(s.download))
	mux.Handle("GET /jobs", page(s.jobs))
	mux.Handle("GET /jobs/{id}", page(s.job))
	mux.Handle("POST /jobs/run", page(s.runNow))
	mux.Handle("GET /settings", page(s.settings))
	mux.Handle("POST /settings/{section}", page(s.saveSettings))
	mux.Handle("GET /audit", page(s.auditPage))

	var h http.Handler = mux
	h = middleware.CSRF(h)
	h = s.Sessions.WithSession(h)
	h = middleware.SecurityHeaders(h)
	h = middleware.LocalOnly(h)
	return h
}

// requireSetup sends logged-in users to the wizard until setup is complete.
func (s *Server) requireSetup(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Svc.SetupComplete() || s.Svc.Runner() == nil {
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

// page is what every template gets.
type page struct {
	Title   string
	Nav     string // active nav item
	CSRF    string
	Flash   string
	Error   string
	Server  string
	Version string
	Wizard  bool // hide the nav
	Refresh int  // seconds; reload the page while something runs
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p page) {
	t := s.pages[name]
	if t == nil {
		http.Error(w, "no template "+name, http.StatusInternalServerError)
		return
	}
	if sess := middleware.From(r); sess != nil {
		p.CSRF = sess.CSRF
		if f := s.Sessions.TakeFlash(sess); f != "" && p.Flash == "" {
			p.Flash = f
		}
	}
	p.Server = s.serverName()
	p.Version = version.Version
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		s.logf("render %s: %v", name, err)
		http.Error(w, "page error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) serverName() string {
	if r := s.Svc.Runner(); r != nil {
		return r.Server
	}
	if n := s.Svc.Config().Server.Name; n != "" {
		return n
	}
	return "this server"
}

// flash sets a message for the next page and redirects there.
func (s *Server) flash(w http.ResponseWriter, r *http.Request, to, msg string) {
	if sess := middleware.From(r); sess != nil {
		s.Sessions.SetFlash(sess, msg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// audit records an action taken in the UI.
func (s *Server) audit(r *http.Request, action, detail string, ok bool) {
	s.auditAs("ui", action, detail, ok)
}

func (s *Server) auditAs(user, action, detail string, ok bool) {
	if err := s.Svc.State.Audit(state.AuditEntry{Time: s.Now(), User: user, Action: action, Detail: detail, OK: ok}); err != nil {
		s.logf("audit: %v", err)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Svc.Log != nil {
		fmt.Fprintf(s.Svc.Log, "%s [ui] %s\n", s.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.logf("%s %s: %v", r.Method, r.URL.Path, err)
	s.render(w, r, "error", page{Title: "Something went wrong", Error: err.Error()})
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
