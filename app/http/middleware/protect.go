// SPDX-License-Identifier: Apache-2.0

package middleware

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
)

// SecurityHeaders sets headers that stop framing, sniffing, referrer leaks, and any
// script or style not served by pgtijori itself.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin, not no-referrer: with no-referrer browsers send "Origin: null" on
		// form posts, which the CSRF check below would refuse.
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// LocalOnly rejects requests whose Host isn't a loopback name. The UI only listens on
// loopback, but without this check a malicious web page could reach it by pointing
// its own domain at 127.0.0.1 (DNS rebinding).
func LocalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "pgtijori only answers to localhost: open it through the SSH tunnel at http://localhost:<port>", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// CSRF checks the form token of every state-changing request, and that the Origin,
// when the browser sends one, is this same host.
func CSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // forms are small
		// "null" means the browser chose not to say (privacy settings, some redirects);
		// the form token below still has to match.
		if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				http.Error(w, "cross-site request refused", http.StatusForbidden)
				return
			}
		}
		sess := From(r)
		if sess == nil || subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
			http.Error(w, "the form expired: go back, reload the page and try again", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
