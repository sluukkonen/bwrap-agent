package app

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// hostAccountName must use the original host environment, never sandbox overrides.
func hostAccountName(environment []string) (string, error) {
	return resolveHostAccount(os.Getuid(), environment, user.LookupId)
}

func resolveHostAccount(uid int, environment []string, lookup func(string) (*user.User, error)) (string, error) {
	valid := func(name string) bool {
		return strings.TrimSpace(name) != "" && !strings.ContainsAny(name, ":\x00\r\n")
	}
	account, err := lookup(strconv.Itoa(uid))
	if err == nil && account != nil && valid(account.Username) {
		return account.Username, nil
	}
	// CGO-free os/user cannot resolve NSS accounts absent from /etc/passwd.
	// Preserve the invoking session's identity instead of synthesizing "agent".
	if name := envValue(environment, "USER", ""); valid(name) {
		return name, nil
	}
	return "", fmt.Errorf("cannot resolve host account for UID %d: no valid account lookup result or inherited USER", uid)
}
