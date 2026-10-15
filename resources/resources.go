// SPDX-License-Identifier: Apache-2.0

// Package resources embeds the UI's templates and static files into the binary.
package resources

import "embed"

// FS holds views/*.html and static/*.
//
//go:embed views static
var FS embed.FS
