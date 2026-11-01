// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/monitor"
)

// ErrRedisPassword: Redis's default user needs its password to set up monitoring.
var ErrRedisPassword = errors.New("Redis needs its password (requirepass) to add the monitoring user; it's used once and never stored")

// redisMonitorACL is the monitor's Redis user: no password (it can't read anything
// secret), allowed only to read Redis's own numbers. No keys, no values, no CONFIG.
const redisMonitorACL = "ACL SETUSER " + monitor.RedisUser + " reset on nopass -@all +info +slowlog|get +ping"

// RedisMonitorAccess adds the monitor's Redis user and saves it (ACL SAVE with an ACL
// file, CONFIG REWRITE without). The password, if Redis has one, goes on redis-cli's
// standard input, never on its command line.
func RedisMonitorAccess(ctx context.Context, run command.Runner, addr, password string) error {
	args := []string{"-s", addr}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		args = []string{"-h", host, "-p", port}
	}
	cli := func(lines ...string) (string, error) {
		if password != "" {
			lines = append([]string{"AUTH " + redisQuote(password)}, lines...)
		}
		out, err := run.Run(ctx, command.Cmd{Name: "redis-cli", Args: args, Stdin: strings.NewReader(strings.Join(lines, "\n") + "\n")})
		text := string(out)
		for _, line := range strings.Split(text, "\n") {
			switch {
			case strings.HasPrefix(line, "NOAUTH"), strings.HasPrefix(line, "WRONGPASS"), strings.Contains(line, "invalid password"):
				return text, ErrRedisPassword
			case strings.HasPrefix(line, "ERR"), strings.HasPrefix(line, "NOPERM"), strings.HasPrefix(line, "(error)"):
				return text, fmt.Errorf("redis: %s", line)
			}
		}
		if err != nil {
			return text, fmt.Errorf("redis-cli: %w\n%s", err, text)
		}
		return text, nil
	}
	out, err := cli(redisMonitorACL, "CONFIG GET aclfile")
	if err != nil {
		return err
	}
	save := "CONFIG REWRITE"
	if lines := strings.Fields(out); len(lines) >= 3 && lines[1] == "aclfile" {
		save = "ACL SAVE" // OK aclfile <path>
	}
	_, err = cli(save)
	return err
}

// redisQuote quotes a value for a redis-cli input line.
func redisQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
