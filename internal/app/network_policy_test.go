package app

import (
	"net/netip"
	"reflect"
	"testing"
)

func mustParseAddress(t *testing.T, value string) netip.Addr {
	t.Helper()
	address, err := netip.ParseAddr(value)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

func TestNetworkOriginNormalizationAndMatching(t *testing.T) {
	values, err := normalizeNetworkAllowList([]string{
		"HTTPS://Example.COM/",
		"https://example.com:443",
		"https://*.Example.COM:8443",
		"https://[2001:DB8::1]",
		"https://[2001:DB8::1]:8443",
		"http://*",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://example.com", "https://*.example.com:8443", "https://[2001:db8::1]", "https://[2001:db8::1]:8443", "http://*"}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("normalized origins = %#v, want %#v", values, want)
	}
	policy, err := parseNetworkPolicy(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []struct {
		scheme string
		host   string
		port   uint16
	}{
		{"https", "example.com", 443},
		{"https", "sub.example.com", 8443},
		{"https", "deep.sub.example.com", 8443},
		{"https", "2001:db8::1", 443},
		{"https", "2001:db8::1", 8443},
		{"http", "anything.invalid", 80},
	} {
		if !policy.allows(allowed.scheme, allowed.host, allowed.port) {
			t.Errorf("expected policy to allow %#v", allowed)
		}
	}
	for _, denied := range []struct {
		scheme string
		host   string
		port   uint16
	}{
		{"http", "anything.invalid", 8080},
		{"https", "example.com", 8443},
		{"https", "other.example.net", 443},
		{"https", "example.com", 80},
	} {
		if policy.allows(denied.scheme, denied.host, denied.port) {
			t.Errorf("expected policy to deny %#v", denied)
		}
	}
}

func TestNetworkOriginValidation(t *testing.T) {
	for _, invalid := range []string{
		"", "ftp://example.com", "https://", "https://user@example.com",
		"https://example.com/path", "https://example.com?query", "https://example.com#fragment",
		"https://*example.com", "https://exa_mple.com", "https://example.com:0",
	} {
		if _, err := parseNetworkOrigin(invalid); err == nil {
			t.Errorf("parseNetworkOrigin(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestEmptyNetworkPolicyDeniesEverything(t *testing.T) {
	policy, err := parseNetworkPolicy(nil)
	if err != nil {
		t.Fatal(err)
	}
	if policy.allows("https", "example.com", 443) {
		t.Fatal("empty policy allowed a destination")
	}
}

func TestSafeProxyDestinationRejectsHostLocalAddresses(t *testing.T) {
	for _, value := range []string{"0.0.0.0", "127.0.0.1", "::", "::1", "169.254.1.2", "fe80::1", "224.0.0.1"} {
		address := mustParseAddress(t, value)
		if safeProxyDestination(address) {
			t.Errorf("protected address %s was accepted", value)
		}
	}
	for _, value := range []string{"10.0.0.1", "192.168.1.1", "203.0.113.1", "2001:db8::1"} {
		address := mustParseAddress(t, value)
		if !safeProxyDestination(address) {
			t.Errorf("routable address %s was rejected", value)
		}
	}
}
