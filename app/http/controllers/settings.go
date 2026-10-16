// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"net/http"
	"strings"

	"github.com/logisticinfotech/pgtijori/config"
)

type settingsData struct {
	Cfg    *config.Config
	Result *testResult
	Errors map[string][]string // per section
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, cfg *config.Config, res *testResult, errs map[string][]string) {
	d := settingsData{Cfg: cfg, Result: res, Errors: errs}
	s.render(w, r, "settings", page{Title: "Settings", Nav: "settings", Data: d})
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.settingsPage(w, r, s.Svc.Config(), nil, nil)
}

// saveSettings handles each section's Save and Test buttons.
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	section := r.PathValue("section")
	cfg := s.Svc.Config()
	if err := applyForm(cfg, section, r.PostForm); err != nil {
		s.settingsPage(w, r, cfg, nil, map[string][]string{section: strings.Split(err.Error(), "\n")})
		return
	}
	if r.PostFormValue("action") == "test" {
		res := s.runTest(r.Context(), cfg, section)
		s.audit(r, "test", section, res.OK)
		s.settingsPage(w, r, cfg, &res, nil)
		return
	}
	if err := cfg.Validate(); err != nil {
		s.settingsPage(w, r, cfg, nil, map[string][]string{section: strings.Split(err.Error(), "\n")})
		return
	}
	if err := s.Svc.SaveSettings(r.Context(), cfg); err != nil {
		s.audit(r, "settings", section, false)
		s.settingsPage(w, r, cfg, nil, map[string][]string{section: {err.Error()}})
		return
	}
	s.audit(r, "settings", "saved "+section, true)
	s.flash(w, r, "/settings#"+section, "Saved. The new settings are in use.")
}
