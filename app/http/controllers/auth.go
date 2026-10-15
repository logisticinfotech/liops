// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/logisticinfotech/pgtijori/app/auth"
	"github.com/logisticinfotech/pgtijori/app/http/middleware"
	"github.com/logisticinfotech/pgtijori/app/state"
)

var usernameRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,31}$`)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if has, _ := s.Svc.State.HasUsers(); !has {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if sess := middleware.From(r); sess != nil && sess.Stage == middleware.LoggedIn {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", page{Title: "Log in", Wizard: true})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(strings.ToLower(r.PostFormValue("username")))
	password := r.PostFormValue("password")
	key := "user:" + username
	if !s.Limiter.Allowed(key) {
		s.auditAs(username, "login", "blocked: too many failed attempts", false)
		s.render(w, r, "login", page{Title: "Log in", Wizard: true, Error: "Too many failed attempts. Wait 15 minutes and try again."})
		return
	}
	u, err := s.Svc.State.GetUser(username)
	var hash []byte
	if err == nil {
		hash = u.PasswordHash
	}
	if !auth.CheckPassword(hash, password) {
		s.Limiter.Fail(key)
		s.auditAs(username, "login", "wrong username or password", false)
		s.render(w, r, "login", page{Title: "Log in", Wizard: true, Error: "Wrong username or password."})
		return
	}
	s.Sessions.Destroy(w, r)
	s.Sessions.Start(w, username, middleware.Password)
	http.Redirect(w, r, "/login/totp", http.StatusSeeOther)
}

type totpData struct {
	Enroll bool
	Secret string
	QR     string
}

func (s *Server) totpPage(w http.ResponseWriter, r *http.Request) {
	sess := middleware.From(r)
	if sess == nil || sess.Stage != middleware.Password {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	u, err := s.Svc.State.GetUser(sess.User)
	if err != nil {
		s.Sessions.Destroy(w, r)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if u.Enrolled() {
		s.render(w, r, "totp", page{Title: "Authenticator code", Wizard: true, Data: totpData{}})
		return
	}
	// First login: show a new secret to enroll. It's kept in the session until a code confirms it.
	e, err := auth.NewEnrollment(s.serverName(), u.Username, sess.PendingTOTP)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Sessions.Update(sess, func(se *middleware.Session) { se.PendingTOTP = e.Secret })
	s.render(w, r, "totp", page{Title: "Set up your authenticator", Wizard: true, Data: totpData{Enroll: true, Secret: e.Secret, QR: e.QR}})
}

func (s *Server) totp(w http.ResponseWriter, r *http.Request) {
	sess := middleware.From(r)
	if sess == nil || sess.Stage != middleware.Password {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	key := "user:" + sess.User
	if !s.Limiter.Allowed(key) {
		s.Sessions.Destroy(w, r)
		s.auditAs(sess.User, "login", "blocked: too many failed attempts", false)
		s.render(w, r, "login", page{Title: "Log in", Wizard: true, Error: "Too many failed attempts. Wait 15 minutes and try again."})
		return
	}

	var enrolled bool
	errWrong := errors.New("wrong code")
	err := s.Svc.State.UpdateUser(sess.User, func(u *state.User) error {
		secret := u.TOTPSecret
		if secret == "" {
			secret = sess.PendingTOTP
		}
		if secret == "" {
			return errWrong
		}
		step, ok := auth.VerifyTOTP(secret, r.PostFormValue("code"), u.TOTPLastStep, s.Now())
		if !ok {
			return errWrong
		}
		enrolled = u.TOTPSecret == ""
		u.TOTPSecret, u.TOTPLastStep, u.LastLogin = secret, step, s.Now()
		return nil
	})
	if err != nil {
		s.Limiter.Fail(key)
		s.auditAs(sess.User, "login", "wrong authenticator code", false)
		if errors.Is(err, errWrong) {
			s.flash(w, r, "/login/totp", "That code didn't work. Check the time on your phone and try the next code.")
			return
		}
		s.fail(w, r, err)
		return
	}

	s.Limiter.Reset(key)
	user := sess.User
	s.Sessions.Destroy(w, r)
	s.Sessions.Start(w, user, middleware.LoggedIn)
	detail := ""
	if enrolled {
		detail = "authenticator enrolled"
	}
	s.auditAs(user, "login", detail, true)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.audit(r, "logout", "", true)
	s.Sessions.Destroy(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "account", page{Title: "Your account", Nav: "account"})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	sess := middleware.From(r)
	current, next := r.PostFormValue("current"), r.PostFormValue("new")
	u, err := s.Svc.State.GetUser(sess.User)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !auth.CheckPassword(u.PasswordHash, current) {
		s.audit(r, "password", "wrong current password", false)
		s.render(w, r, "account", page{Title: "Your account", Nav: "account", Error: "The current password is wrong."})
		return
	}
	if next != r.PostFormValue("confirm") {
		s.render(w, r, "account", page{Title: "Your account", Nav: "account", Error: "The new passwords don't match."})
		return
	}
	if err := auth.CheckNewPassword(next); err != nil {
		s.render(w, r, "account", page{Title: "Your account", Nav: "account", Error: capitalize(err.Error()) + "."})
		return
	}
	hash, err := auth.HashPassword(next)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Svc.State.UpdateUser(sess.User, func(u *state.User) error { u.PasswordHash = hash; return nil }); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Sessions.DestroyUser(sess.User, sess) // log out the user's other browsers
	s.audit(r, "password", "changed", true)
	s.flash(w, r, "/account", "Your password was changed. Other sessions were logged out.")
}

// addUser creates a user with a random password, shown once. The new user sets up
// their authenticator at first login.
func (s *Server) addUser(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(strings.ToLower(r.PostFormValue("username")))
	if !usernameRE.MatchString(username) {
		s.flash(w, r, "/settings#users", "Usernames are 2–32 characters: lower-case letters, digits, dot, dash, underscore; starting with a letter.")
		return
	}
	pw := auth.RandomPassword()
	hash, err := auth.HashPassword(pw)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	err = s.Svc.State.CreateUser(&state.User{Username: username, PasswordHash: hash, Created: s.Now()})
	if errors.Is(err, state.ErrExists) {
		s.flash(w, r, "/settings#users", "A user named "+username+" already exists.")
		return
	} else if err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "user", "added "+username, true)
	s.flash(w, r, "/settings#users", "Added "+username+". Their password is "+pw+" — it's shown only now. They set up their authenticator at first login.")
}

// resetUser gives a user a new random password and makes them enroll a new authenticator.
func (s *Server) resetUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("name")
	pw := auth.RandomPassword()
	hash, err := auth.HashPassword(pw)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	err = s.Svc.State.UpdateUser(username, func(u *state.User) error {
		u.PasswordHash, u.TOTPSecret, u.TOTPLastStep = hash, "", 0
		return nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Sessions.DestroyUser(username, middleware.From(r))
	s.audit(r, "user", "reset "+username, true)
	s.flash(w, r, "/settings#users", "Reset "+username+". Their new password is "+pw+" — it's shown only now. They set up a new authenticator at next login.")
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("name")
	if username == middleware.From(r).User {
		s.flash(w, r, "/settings#users", "You can't delete yourself.")
		return
	}
	if err := s.Svc.State.DeleteUser(username); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Sessions.DestroyUser(username, nil)
	s.audit(r, "user", "deleted "+username, true)
	s.flash(w, r, "/settings#users", "Deleted "+username+".")
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
