// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/config"
)

// devEnv turns on development mode, for working on liops on a laptop (a Mac with
// Homebrew PostgreSQL, for example). It is not a production setting: never set it on
// a server. See docs/development.md.
const devEnv = "LIOPS_DEV"

// devMode reports whether LIOPS_DEV=1. In development mode commands don't need
// root, files default to ./dev/ instead of /etc, /var/lib and /var/log, and
// `setup --skip-pgbackrest` is allowed.
func devMode() bool { return os.Getenv(devEnv) == "1" }

// defaultPaths are the file locations used when --config, --state and --log aren't given.
func defaultPaths() paths {
	if devMode() {
		return paths{config: "dev/config.yaml", state: "dev/state.db", log: "dev/liops.log"}
	}
	return paths{config: config.DefaultPath, state: state.DefaultPath, log: defaultLogPath}
}
