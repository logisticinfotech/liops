// SPDX-License-Identifier: Apache-2.0

package state

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// User is a UI account. Passwords are bcrypt hashes; TOTP is mandatory.
type User struct {
	Username     string    `json:"username"`
	PasswordHash []byte    `json:"password_hash"`
	TOTPSecret   string    `json:"totp_secret,omitempty"` // empty until the user enrolls
	TOTPLastStep int64     `json:"totp_last_step,omitempty"`
	Created      time.Time `json:"created"`
	LastLogin    time.Time `json:"last_login,omitzero"`
}

// Enrolled reports whether the user has set up TOTP.
func (u *User) Enrolled() bool { return u.TOTPSecret != "" }

// ErrExists is returned when creating a user that already exists.
var ErrExists = errors.New("already exists")

// CreateUser adds a user. It fails if the username is taken.
func (s *Store) CreateUser(u *User) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		if b.Get([]byte(u.Username)) != nil {
			return ErrExists
		}
		return putJSON(b, []byte(u.Username), u)
	})
}

// SaveUser stores an existing user again.
func (s *Store) SaveUser(u *User) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		if b.Get([]byte(u.Username)) == nil {
			return ErrNotFound
		}
		return putJSON(b, []byte(u.Username), u)
	})
}

// UpdateUser loads a user, applies fn and saves the result in one transaction, so
// e.g. the TOTP replay check can't race with another login.
func (s *Store) UpdateUser(username string, fn func(*User) error) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketUsers)
		v := b.Get([]byte(username))
		if v == nil {
			return ErrNotFound
		}
		var u User
		if err := json.Unmarshal(v, &u); err != nil {
			return err
		}
		if err := fn(&u); err != nil {
			return err
		}
		return putJSON(b, []byte(username), &u)
	})
}

// GetUser returns a user, or ErrNotFound.
func (s *Store) GetUser(username string) (*User, error) {
	var u *User
	err := s.view(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketUsers).Get([]byte(username))
		if v == nil {
			return ErrNotFound
		}
		u = &User{}
		return json.Unmarshal(v, u)
	})
	return u, err
}

// Users returns all users, sorted by name.
func (s *Store) Users() ([]User, error) {
	var users []User
	err := s.view(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketUsers).ForEach(func(k, v []byte) error {
			var u User
			if err := json.Unmarshal(v, &u); err == nil {
				users = append(users, u)
			}
			return nil
		})
	})
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	return users, err
}

// DeleteUser removes a user.
func (s *Store) DeleteUser(username string) error {
	return s.update(func(tx *bolt.Tx) error { return tx.Bucket(bucketUsers).Delete([]byte(username)) })
}

// HasUsers reports whether any user exists. With none, the UI only offers the setup wizard.
func (s *Store) HasUsers() (bool, error) {
	has := false
	err := s.view(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket(bucketUsers).Cursor().First()
		has = k != nil
		return nil
	})
	return has, err
}
