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
	for _, host := range []string{"example.com", "sub.example.com", "deep.sub.example.com", "anything.invalid"} {
		if !policy.allowsHostname(host) {
			t.Errorf("expected policy to allow DNS for %q", host)
		}
	}
	restricted, err := parseNetworkPolicy([]string{"https://example.com", "https://*.example.com:8443"})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"other.example.net", "notexample.com"} {
		if restricted.allowsHostname(host) {
			t.Errorf("expected restricted policy to deny DNS for %q", host)
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
	if policy.allows("https", "example.com", 443) || policy.allows("http", "example.com", 80) || policy.allowsHostname("example.com") {
		t.Fatal("empty policy allowed a destination")
	}
}

func TestNetworkPolicyHostnameMatching(t *testing.T) {
	for _, test := range []struct {
		name    string
		origins []string
		host    string
		allowed bool
	}{
		{"empty", nil, "example.com", false},
		{"exact", []string{"https://example.com:8443"}, "example.com", true},
		{"normalized", []string{"https://Example.COM."}, "EXAMPLE.com.", true},
		{"http", []string{"http://example.com:8080"}, "example.com", true},
		{"exact excludes children", []string{"https://example.com"}, "sub.example.com", false},
		{"wildcard child", []string{"https://*.example.com"}, "sub.example.com", true},
		{"wildcard descendant", []string{"https://*.example.com"}, "deep.sub.example.com", true},
		{"wildcard normalized", []string{"https://*.example.com"}, "SUB.EXAMPLE.COM.", true},
		{"wildcard excludes apex", []string{"https://*.example.com"}, "example.com", false},
		{"misleading suffix", []string{"https://*.example.com"}, "notexample.com", false},
		{"misleading prefix", []string{"https://*.example.com"}, "example.com.evil.test", false},
		{"full wildcard", []string{"http://*"}, "unlisted.example", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, err := parseNetworkPolicy(test.origins)
			if err != nil {
				t.Fatal(err)
			}
			if got := policy.allowsHostname(test.host); got != test.allowed {
				t.Fatalf("allowsHostname(%q) = %t, want %t", test.host, got, test.allowed)
			}
		})
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
