// SPDX-License-Identifier: Apache-2.0

// Package middleware protects the UI. There is no login: reaching the UI means having
// an SSH tunnel to the server (it listens on loopback only). What's left guards
// against other web pages in the same browser: CSRF tokens, a Host check against DNS
// rebinding, and security headers.
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
const CookieName = "liops_session"

// Session is one browser's session. It only carries the CSRF token and a one-time
// message. Sessions live in memory.
type Session struct {
	ID   string
	CSRF string
	// Flash is a one-time message shown on the next page.
	Flash string
	Seen  time.Time
}

// Sessions stores sessions.
type Sessions struct {
	mu sync.Mutex
	m  map[string]*Session
	// Idle is how long an unused session lives.
	Idle time.Duration
	Now  func() time.Time
}

// maxSessions bounds memory use.
const maxSessions = 1000

func NewSessions() *Sessions {
	return &Sessions{m: map[string]*Session{}, Idle: 12 * time.Hour, Now: time.Now}
}

func token() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on Linux
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Start creates a session and sets its cookie.
func (s *Sessions) Start(w http.ResponseWriter) *Session {
	now := s.Now()
	sess := &Session{ID: token(), CSRF: token(), Seen: now}
	s.mu.Lock()
	for id, old := range s.m {
		if now.Sub(old.Seen) > s.Idle {
			delete(s.m, id)
		}
	}
	if len(s.m) >= maxSessions {
		var oldest *Session
		for _, old := range s.m {
			if oldest == nil || old.Seen.Before(oldest.Seen) {
				oldest = old
			}
		}
		delete(s.m, oldest.ID)
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
	if now.Sub(sess.Seen) > s.Idle {
		delete(s.m, sess.ID)
		return nil
	}
	sess.Seen = now
	return sess
}

// SetFlash sets the message shown on the next page.
func (s *Sessions) SetFlash(sess *Session, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.Flash = msg
}

// TakeFlash returns and clears the session's flash message.
func (s *Sessions) TakeFlash(sess *Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := sess.Flash
	sess.Flash = ""
	return f
}

type ctxKey struct{}

// WithSession makes sure every request has a session, so every form carries a CSRF token.
func (s *Sessions) WithSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := s.Get(r)
		if sess == nil {
			sess = s.Start(w)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	})
}

// From returns the request's session (set by WithSession).
func From(r *http.Request) *Session {
	sess, _ := r.Context().Value(ctxKey{}).(*Session)
	return sess
}
