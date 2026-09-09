//go:build linux && !amd64 && !arm64

package app

import (
	"strings"
	"testing"
)

func TestUnsupportedArchitectureSeccompFallback(t *testing.T) {
	probe := func() (bool, error) {
		t.Fatal("seccomp support probe was called on an unsupported architecture")
		return false, nil
	}
	status, warning, err := determineSeccompStatus("auto", false, probe)
	if err != nil || status.Requested != "auto" || status.Effective != "unsupported" || status.Profile != "" || warning == "" {
		t.Fatalf("automatic fallback = %#v, %q, %v", status, warning, err)
	}
	if _, _, err := determineSeccompStatus("required", false, probe); err == nil || !strings.Contains(err.Error(), "architecture") {
		t.Fatalf("required fallback error = %v", err)
	}
	status, warning, err = determineSeccompStatus("off", false, probe)
	if err != nil || status.Requested != "off" || status.Effective != "off" || warning != "" {
		t.Fatalf("disabled fallback = %#v, %q, %v", status, warning, err)
	}
}
