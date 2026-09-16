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
		{"IPv4", []string{"https://203.0.113.1"}, "203.0.113.1", true},
		{"other IPv4", []string{"https://203.0.113.1"}, "203.0.113.2", false},
		{"IPv6 normalized", []string{"https://[2001:db8::a]"}, "2001:DB8::A", true},
		{"other IPv6", []string{"https://[2001:db8::a]"}, "2001:db8::b", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, err := parseNetworkPolicy(test.origins)
			if err != nil {
				t.Fatal(err)
			}
			if got := policy.allowsHostname(test.host); got != test.allowed {
				t.Fatalf("allowsHostname(%q) = %t, want %t", test.host, got, test.allowed)
			}
			for _, origin := range policy.origins {
				if got := policy.allows(origin.scheme, test.host, origin.port); got != test.allowed {
					t.Errorf("allows(%q, %q, %d) = %t, want %t", origin.scheme, test.host, origin.port, got, test.allowed)
				}
				if policy.allows("ftp", test.host, origin.port) {
					t.Error("HTTP policy ignored scheme restriction")
				}
				if policy.allows(origin.scheme, test.host, 65535) {
					t.Error("HTTP policy ignored port restriction")
				}
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

func TestNetworkAllowListCanonicalization(t *testing.T) {
	for _, test := range []struct {
		name  string
		input []string
		want  []string
	}{
		{"nil", nil, []string{}},
		{"empty", []string{}, []string{}},
		{"order and duplicates", []string{
			"https://Z.example:443/", "http://a.example:80", "https://z.example",
			"https://*.Example.COM:8443/", "http://a.example/", "https://*.example.com:8443",
		}, []string{"https://z.example", "http://a.example", "https://*.example.com:8443"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeNetworkAllowList(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("normalized = %#v, want %#v", got, test.want)
			}
			again, err := normalizeNetworkAllowList(got)
			if err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("normalization not idempotent: %#v, %v", again, err)
			}
		})
	}
}

func TestNetworkAllowListReportsFirstInvalidOrigin(t *testing.T) {
	values := []string{"https://valid.example", "ftp://invalid.example", "https://"}
	want := `invalid origin "ftp://invalid.example": scheme must be http or https`
	if normalized, err := normalizeNetworkAllowList(values); err == nil || err.Error() != want || normalized != nil {
		t.Fatalf("normalization = %#v, %v; want nil, %q", normalized, err, want)
	}
	if policy, err := parseNetworkPolicy(values); err == nil || err.Error() != want || len(policy.origins) != 0 {
		t.Fatalf("policy = %#v, %v; want empty, %q", policy, err, want)
	}
}
