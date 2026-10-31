// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"net/http"
	"slices"
	"strings"

	"github.com/logisticinfotech/liops/config"
)

type settingsData struct {
	Cfg    *config.Config
	Result *testResult
	Errors map[string][]string // per section
	Tabs   []settingsTab
	Tab    string // the tab shown
}

// settingsTab is one tab of the Settings page and the sections (forms) on it.
type settingsTab struct {
	ID, Title string
	Sections  []string
}

var settingsTabs = []settingsTab{
	{"server", "Server", []string{sectionServer}},
	{"storage", "Storage", []string{sectionStorage}},
	{"email", "Email", []string{sectionEmail}},
	{"backups", "Backups", []string{sectionSchedules}},
	{"dumps", "Dumps", []string{sectionDumps, sectionLabels}},
	{"monitoring", "Monitoring", []string{sectionMonitor}},
}

// tabOf returns the tab a section's form is on.
func tabOf(section string) string {
	for _, t := range settingsTabs {
		if slices.Contains(t.Sections, section) {
			return t.ID
		}
	}
	return settingsTabs[0].ID
}

func (s *Server) settingsPage(w http.ResponseWriter, r *http.Request, tab string, cfg *config.Config, res *testResult, errs map[string][]string) {
	if !slices.ContainsFunc(settingsTabs, func(t settingsTab) bool { return t.ID == tab }) {
		tab = settingsTabs[0].ID
	}
	d := settingsData{Cfg: cfg, Result: res, Errors: errs, Tabs: settingsTabs, Tab: tab}
	s.render(w, r, "settings", page{Title: "Settings", Nav: "settings", Data: d})
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	s.settingsPage(w, r, r.URL.Query().Get("tab"), s.Svc.Config(), nil, nil)
}

// saveSettings handles each section's Save and Test buttons.
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	section := r.PathValue("section")
	tab := tabOf(section)
	cfg := s.Svc.Config()
	if err := applyForm(cfg, section, r.PostForm); err != nil {
		s.settingsPage(w, r, tab, cfg, nil, map[string][]string{section: strings.Split(err.Error(), "\n")})
		return
	}
	if r.PostFormValue("action") == "test" {
		res := s.runTest(r.Context(), cfg, section)
		s.audit(r, "test", section, res.OK)
		s.settingsPage(w, r, tab, cfg, &res, nil)
		return
	}
	if err := cfg.Validate(); err != nil {
		s.settingsPage(w, r, tab, cfg, nil, map[string][]string{section: strings.Split(err.Error(), "\n")})
		return
	}
	if err := s.Svc.SaveSettings(r.Context(), cfg); err != nil {
		s.audit(r, "settings", section, false)
		s.settingsPage(w, r, tab, cfg, nil, map[string][]string{section: {err.Error()}})
		return
	}
	s.audit(r, "settings", "saved "+section, true)
	s.flash(w, r, "/settings?tab="+tab, "Saved. The new settings are in use.")
}
