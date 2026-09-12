package app

import (
	"errors"
	"os/user"
	"strings"
	"testing"
)

func TestResolveHostAccount(t *testing.T) {
	for _, tc := range []struct {
		name, local string
		environment []string
		lookupFails bool
		want        string
	}{
		{name: "local wins", local: "local-user", environment: []string{"USER=other"}, want: "local-user"},
		{name: "local without environment", local: "local-user", want: "local-user"},
		{name: "NSS inherited user", lookupFails: true, environment: []string{"USER=ldap-user", "LOGNAME=other"}, want: "ldap-user"},
		{name: "missing fallback", lookupFails: true},
		{name: "empty fallback", lookupFails: true, environment: []string{"USER="}},
		{name: "whitespace fallback", lookupFails: true, environment: []string{"USER= "}},
		{name: "LOGNAME is not USER", lookupFails: true, environment: []string{"LOGNAME=ldap-user"}},
		{name: "invalid fallback", lookupFails: true, environment: []string{"USER=ldap\nroot"}},
		{name: "invalid local and fallback", local: "local:user", environment: []string{"USER=ldap\x00user"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := func(uid string) (*user.User, error) {
				if uid != "12345" {
					t.Fatalf("looked up UID %s", uid)
				}
				if tc.lookupFails {
					return nil, errors.New("account is absent from passwd")
				}
				return &user.User{Uid: uid, Username: tc.local}, nil
			}
			got, err := resolveHostAccount(12345, tc.environment, lookup)
			if got != tc.want || (err != nil) != (tc.want == "") {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
			if err != nil && !strings.Contains(err.Error(), "12345") {
				t.Fatalf("error lacks UID: %v", err)
			}
		})
	}
}
