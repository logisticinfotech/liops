// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/logisticinfotech/liops/app/command"
)

func TestRedisMonitorAccess(t *testing.T) {
	var inputs []string
	var argv []string
	fake := func(needPass bool, aclfile string) *command.Fake {
		return &command.Fake{Handle: func(p string, c command.Cmd) ([]byte, error) {
			in, _ := io.ReadAll(c.Stdin)
			inputs = append(inputs, string(in))
			argv = append(argv, strings.Join(c.Args, " "))
			if needPass && !strings.HasPrefix(string(in), "AUTH ") {
				return []byte("NOAUTH Authentication required.\n"), nil
			}
			if strings.Contains(string(in), "CONFIG GET aclfile") {
				return []byte("OK\naclfile\n" + aclfile + "\n"), nil
			}
			return []byte("OK\n"), nil
		}}
	}
	if err := RedisMonitorAccess(context.Background(), fake(true, ""), "127.0.0.1:6379", ""); !errors.Is(err, ErrRedisPassword) {
		t.Errorf("no password: %v", err)
	}
	inputs, argv = nil, nil
	if err := RedisMonitorAccess(context.Background(), fake(true, ""), "127.0.0.1:6379", `p"w`); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inputs[0], `AUTH "p\"w"`+"\n"+"ACL SETUSER liops-monitor reset on nopass -@all +info +slowlog|get +ping") || !strings.Contains(inputs[1], "CONFIG REWRITE") {
		t.Errorf("inputs = %q", inputs)
	}
	for _, a := range argv {
		if strings.Contains(a, "p\"w") || a != "-h 127.0.0.1 -p 6379" {
			t.Errorf("the password must never be on the command line: %q", a)
		}
	}
	inputs = nil
	if err := RedisMonitorAccess(context.Background(), fake(false, "/etc/redis/users.acl"), "/run/redis/redis-server.sock", ""); err != nil || !strings.Contains(inputs[1], "ACL SAVE") {
		t.Errorf("with an ACL file: %v %q", err, inputs)
	}
}
