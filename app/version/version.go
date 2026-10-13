// SPDX-License-Identifier: Apache-2.0

// Package version holds build information, set at release time with -ldflags.
package version

import "fmt"

// Set by the release build:
//
//	-ldflags "-X github.com/logisticinfotech/pgtijori/app/version.Version=v1.2.0
//	          -X github.com/logisticinfotech/pgtijori/app/version.Commit=<sha>"
var (
	Version = "dev"
	Commit  = "none"
)

// String returns a one-line description, e.g. "pgtijori v1.2.0 (abc1234)".
func String() string {
	return fmt.Sprintf("pgtijori %s (%s)", Version, Commit)
}
