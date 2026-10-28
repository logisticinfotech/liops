// SPDX-License-Identifier: Apache-2.0

package notify

import "strings"

// reasons maps known error text to a plain-language likely reason. The first match
// wins, so more specific patterns come first. Patterns are lower case.
var reasons = []struct {
	patterns []string
	reason   string
}{
	{[]string{"no space left on device"}, "the server disk is full"},
	{[]string{"not archived before the", "archive-push-queue-max"}, "WAL upload is stuck: PostgreSQL can't hand its changes to pgBackRest"},
	{[]string{"nosuchbucket", "bucket does not exist", "specified bucket does not exist"}, "the storage bucket doesn't exist"},
	{[]string{"invalidaccesskeyid", "signaturedoesnotmatch", "access denied", "accessdenied", "403 forbidden", "403 (forbidden)", "status code 403", "http 403"}, "the storage key was rejected"},
	{[]string{"x509:", "certificate verify failed", "certificate is not valid"}, "the storage server's TLS certificate wasn't trusted"},
	{[]string{"do not match the database", "do not match stanza", "system-id"}, "the backups in this folder belong to a different PostgreSQL server (each server needs its own backup name; see docs/restore.md section 3)"},
	{[]string{"does not exist in repo", "missing stanza", "stanza-create"}, "the pgBackRest stanza hasn't been created (run: liops setup)"},
	{[]string{"unable to acquire lock"}, "another pgBackRest command was already running"},
	{[]string{"could not connect to server", "connection to server on socket", "connection refused", "the database system is starting up", "the database system is shutting down"}, "the database refused the connection (is PostgreSQL running?)"},
	{[]string{"no such host", "i/o timeout", "timed out", "timeout", "connection reset", "network is unreachable"}, "couldn't reach storage"},
	{[]string{"permission denied"}, "a file or directory has the wrong permissions"},
	{[]string{"out of memory", "cannot allocate memory"}, "the server ran out of memory"},
}

// LikelyReason guesses, in plain language, why a command failed from its output.
func LikelyReason(output string) string {
	lower := strings.ToLower(output)
	for _, r := range reasons {
		for _, p := range r.patterns {
			if strings.Contains(lower, p) {
				return r.reason
			}
		}
	}
	return "unknown — see technical details"
}
