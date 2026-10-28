// SPDX-License-Identifier: Apache-2.0

// Package version holds build information, set at release time with -ldflags.
package version

import "fmt"

// Set by the release build:
//
//	-ldflags "-X github.com/logisticinfotech/liops/app/version.Version=v1.2.0
//	          -X github.com/logisticinfotech/liops/app/version.Commit=<sha>"
var (
	Version = "dev"
	Commit  = "none"
)

// String returns a one-line description, e.g. "liops v1.2.0 (abc1234)".
func String() string {
	return fmt.Sprintf("liops %s (%s)", Version, Commit)
}
