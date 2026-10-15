// SPDX-License-Identifier: Apache-2.0

// Package middleware protects the UI: sessions with mandatory TOTP, CSRF tokens,
// login rate limiting, security headers, and a Host check that keeps the UI reachable
// only as localhost (through the SSH tunnel).
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"
)

// CookieName is the session cookie.
const CookieName = "pgtijori_session"

// Session stages.
const (
	Anonymous = iota // not logged in; still carries a CSRF token for the login form
	Password         // password checked, TOTP not yet (or being enrolled)
	LoggedIn         // password and TOTP checked
)

// Session is one browser's session. Sessions live in memory: restarting pgtijori
// logs everyone out, which is fine for a tool used now and then.
type Session struct {
	ID    string
	CSRF  string
	User  string
	Stage int
	// PendingTOTP is a TOTP secret shown during enrollment, until a code confirms it.
	PendingTOTP string
	// SetupUnlocked: the setup token was entered, so the first admin can be created.
	SetupUnlocked bool
	// Flash is a one-time message shown on the next page.
	Flash   string
	Created time.Time
	Seen    time.Time
}

// Sessions stores sessions.
type Sessions struct {
	mu sync.Mutex
	m  map[string]*Session
	// Idle and Max are the inactivity and absolute timeouts.
	Idle, Max time.Duration
	Now       func() time.Time
}

// maxSessions bounds memory use: anonymous sessions are created per visitor.
const maxSessions = 1000

func NewSessions() *Sessions {
	return &Sessions{m: map[string]*Session{}, Idle: 30 * time.Minute, Max: 12 * time.Hour, Now: time.Now}
}

func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on Linux
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Start creates a fresh session (a new ID, so a session can't be fixed by an
// attacker) and sets its cookie.
func (s *Sessions) Start(w http.ResponseWriter, user string, stage int) *Session {
	now := s.Now()
	sess := &Session{ID: token(), CSRF: token(), User: user, Stage: stage, Created: now, Seen: now}
	s.mu.Lock()
	s.expireLocked(now)
	if len(s.m) >= maxSessions {
		s.evictOldestLocked()
	}
	s.m[sess.ID] = sess
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		// Not Secure: the UI is plain HTTP on loopback, encrypted by the SSH tunnel.
	})
	return sess
}

// Get returns the request's session, or nil.
func (s *Sessions) Get(r *http.Request) *Session {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return nil
	}
	now := s.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[c.Value]
	if sess == nil {
		return nil
	}
	if now.Sub(sess.Seen) > s.Idle || now.Sub(sess.Created) > s.Max {
		delete(s.m, sess.ID)
		return nil
	}
	sess.Seen = now
	return sess
}

// Update applies fn to a session under the lock.
func (s *Sessions) Update(sess *Session, fn func(*Session)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(sess)
}

// TakeFlash returns and clears the session's flash message.
func (s *Sessions) TakeFlash(sess *Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := sess.Flash
	sess.Flash = ""
	return f
}

// Destroy ends the request's session and clears its cookie.
func (s *Sessions) Destroy(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		s.mu.Lock()
		delete(s.m, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// DestroyUser ends every session of a user (after a password change or deletion).
// keep, if not nil, survives.
func (s *Sessions) DestroyUser(user string, keep *Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.m {
		if sess.User == user && sess != keep {
			delete(s.m, id)
		}
	}
}

func (s *Sessions) expireLocked(now time.Time) {
	for id, sess := range s.m {
		if now.Sub(sess.Seen) > s.Idle || now.Sub(sess.Created) > s.Max {
			delete(s.m, id)
		}
	}
}

func (s *Sessions) evictOldestLocked() {
	var oldest *Session
	for _, sess := range s.m {
		if sess.Stage == Anonymous && (oldest == nil || sess.Seen.Before(oldest.Seen)) {
			oldest = sess
		}
	}
	if oldest != nil {
		delete(s.m, oldest.ID)
	}
}

type ctxKey struct{}

// WithSession makes sure every request has a session (anonymous if needed), so every
// form, including login, carries a CSRF token.
func (s *Sessions) WithSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := s.Get(r)
		if sess == nil {
			sess = s.Start(w, "", Anonymous)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	})
}

// From returns the request's session (set by WithSession).
func From(r *http.Request) *Session {
	sess, _ := r.Context().Value(ctxKey{}).(*Session)
	return sess
}

// RequireLogin redirects to the login page unless the session passed TOTP.
func RequireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sess := From(r); sess == nil || sess.Stage != LoggedIn {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
