// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

func TestLocalOnly(t *testing.T) {
	for host, want := range map[string]int{
		"localhost:8899":     200,
		"127.0.0.1:8899":     200,
		"[::1]:8899":         200,
		"evil.example.com":   421,
		"10.0.0.5:8899":      421,
		"localhost.evil.com": 421,
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		LocalOnly(ok).ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: got %d, want %d", host, rec.Code, want)
		}
	}
}

func TestCSRF(t *testing.T) {
	s := NewSessions()
	rec := httptest.NewRecorder()
	sess := s.Start(rec)
	cookie := rec.Result().Cookies()[0]
	h := s.WithSession(CSRF(ok))

	post := func(token, origin string) int {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(url.Values{"csrf": {token}}.Encode()))
		req.Host = "localhost:8899"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(sess.CSRF, "http://localhost:8899"); code != 200 {
		t.Errorf("valid token: %d", code)
	}
	if code := post("wrong", ""); code != 403 {
		t.Errorf("wrong token: %d", code)
	}
	if code := post(sess.CSRF, "http://evil.example.com"); code != 403 {
		t.Errorf("foreign origin: %d", code)
	}
}

func TestSessionExpiry(t *testing.T) {
	s := NewSessions()
	now := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	rec := httptest.NewRecorder()
	sess := s.Start(rec)
	cookie := rec.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("cookie flags: %+v", cookie)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	if s.Get(req) != sess {
		t.Fatal("session not found")
	}
	now = now.Add(13 * time.Hour)
	if s.Get(req) != nil {
		t.Error("an idle session must expire")
	}
}
