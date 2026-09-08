package app

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

const (
	proxyGuestPort         = 65532
	proxyContainerPort     = proxyGuestPort
	proxyPortPlaceholder   = "__BWRAP_AGENT_PROXY_HOST_PORT__"
	proxyContainerHostname = "host.containers.internal"
	proxyContainerAddress  = "169.254.1.2"
)

type networkOrigin struct {
	scheme   string
	host     string
	port     uint16
	wildcard bool
	allHosts bool
}

type networkPolicy struct {
	origins []networkOrigin
}

func appendUnique(target []string, values ...string) []string {
	seen := make(map[string]struct{}, len(target)+len(values))
	for _, value := range target {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, found := seen[value]; found {
			continue
		}
		seen[value] = struct{}{}
		target = append(target, value)
	}
	return target
}

func normalizeNetworkAllowList(values []string) ([]string, error) {
	result := make([]string, 0, len(values))
	for _, value := range values {
		origin, err := parseNetworkOrigin(value)
		if err != nil {
			return nil, fmt.Errorf("invalid origin %q: %w", value, err)
		}
		result = appendUnique(result, origin.String())
	}
	return result, nil
}

func parseNetworkPolicy(values []string) (networkPolicy, error) {
	policy := networkPolicy{origins: make([]networkOrigin, 0, len(values))}
	for _, value := range values {
		origin, err := parseNetworkOrigin(value)
		if err != nil {
			return networkPolicy{}, fmt.Errorf("invalid origin %q: %w", value, err)
		}
		policy.origins = append(policy.origins, origin)
	}
	return policy, nil
}

func parseNetworkOrigin(value string) (networkOrigin, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return networkOrigin{}, err
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return networkOrigin{}, errorsText("scheme must be http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" {
		return networkOrigin{}, errorsText("origins must not contain credentials, paths, queries, or fragments")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "" {
		return networkOrigin{}, errorsText("hostname is required")
	}
	origin := networkOrigin{scheme: scheme}
	switch {
	case host == "*":
		origin.allHosts = true
	case strings.HasPrefix(host, "*."):
		origin.wildcard = true
		origin.host = strings.TrimPrefix(host, "*.")
		if err := validateNetworkHostname(origin.host); err != nil {
			return networkOrigin{}, err
		}
	default:
		origin.host = host
		if err := validateNetworkHostname(host); err != nil {
			return networkOrigin{}, err
		}
	}
	portText := parsed.Port()
	if portText == "" {
		if scheme == "http" {
			origin.port = 80
		} else {
			origin.port = 443
		}
	} else {
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || port == 0 {
			return networkOrigin{}, errorsText("port must be between 1 and 65535")
		}
		origin.port = uint16(port)
	}
	return origin, nil
}

type errorsText string

func (e errorsText) Error() string { return string(e) }

func validateNetworkHostname(host string) error {
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Zone() != "" {
			return errorsText("scoped IP addresses are not supported")
		}
		return nil
	}
	if len(host) > 253 || strings.ContainsAny(host, ":%") {
		return errorsText("invalid hostname")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errorsText("invalid hostname")
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return errorsText("hostnames must use ASCII letters, digits, dots, and hyphens")
			}
		}
	}
	return nil
}

func (origin networkOrigin) String() string {
	host := origin.host
	isIPv6 := false
	if origin.allHosts {
		host = "*"
	} else if origin.wildcard {
		host = "*." + host
	} else if address, err := netip.ParseAddr(host); err == nil && address.Is6() {
		isIPv6 = true
	}
	defaultPort := uint16(443)
	if origin.scheme == "http" {
		defaultPort = 80
	}
	if origin.port != defaultPort {
		host = net.JoinHostPort(host, strconv.Itoa(int(origin.port)))
	} else if isIPv6 {
		host = "[" + host + "]"
	}
	return origin.scheme + "://" + host
}

func (policy networkPolicy) allows(scheme, host string, port uint16) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, origin := range policy.origins {
		if origin.scheme != scheme || origin.port != port {
			continue
		}
		if origin.allHosts || !origin.wildcard && origin.host == host || origin.wildcard && strings.HasSuffix(host, "."+origin.host) {
			return true
		}
	}
	return false
}

func originTarget(scheme, hostport string) (string, uint16, error) {
	defaultPort := "443"
	if scheme == "http" {
		defaultPort = "80"
	}
	host := hostport
	portText := defaultPort
	if parsedHost, parsedPort, err := net.SplitHostPort(hostport); err == nil {
		host, portText = parsedHost, parsedPort
	} else if strings.Contains(hostport, ":") {
		unbracketed := strings.Trim(hostport, "[]")
		if address, addressErr := netip.ParseAddr(unbracketed); addressErr != nil || !address.Is6() {
			return "", 0, errorsText("invalid host and port")
		}
		host = unbracketed
	}
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if err := validateNetworkHostname(host); err != nil {
		return "", 0, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", 0, errorsText("port must be between 1 and 65535")
	}
	return host, uint16(port), nil
}
