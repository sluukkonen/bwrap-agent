package app

import (
	"fmt"
	"strconv"
	"strings"
)

func parseHostPort(value string) (PortMapping, error) {
	main, protocol, found := strings.Cut(value, "/")
	if !found {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return PortMapping{}, fmt.Errorf("unsupported protocol in --host-port %q", value)
	}
	guestText, hostText, mapped := strings.Cut(main, ":")
	if !mapped {
		hostText = guestText
	}
	parse := func(text string) (int, error) {
		// ParseUint rejects signs, addresses, ranges, and extra separators.
		number, err := strconv.ParseUint(text, 10, 16)
		if err != nil || number == 0 {
			return 0, fmt.Errorf("invalid --host-port %q: ports must be integers between 1 and 65535", value)
		}
		return int(number), nil
	}
	guest, err := parse(guestText)
	if err != nil {
		return PortMapping{}, err
	}
	host, err := parse(hostText)
	if err != nil {
		return PortMapping{}, err
	}
	return PortMapping{Protocol: protocol, Guest: guest, Host: host}, nil
}

func resolveHostPorts(values []string, network string, published []PortMapping) ([]PortMapping, error) {
	if len(values) > 0 && network != "private" {
		return nil, fmt.Errorf("--host-port requires --network private")
	}
	type endpoint struct {
		protocol string
		port     int
	}
	occupied := map[endpoint]string{
		{"tcp", dnsGuestPort}: "sandbox DNS", {"udp", dnsGuestPort}: "sandbox DNS",
		{"tcp", proxyGuestPort}: "sandbox HTTP proxy",
	}
	for _, port := range published {
		occupied[endpoint{port.Protocol, port.Guest}] = "published guest port"
	}
	seen := make(map[endpoint]int)
	result := make([]PortMapping, 0, len(values))
	for _, value := range values {
		mapping, err := parseHostPort(value)
		if err != nil {
			return nil, err
		}
		key := endpoint{mapping.Protocol, mapping.Guest}
		if reason, found := occupied[key]; found {
			return nil, fmt.Errorf("--host-port %q conflicts with %s", value, reason)
		}
		if host, found := seen[key]; found {
			if host != mapping.Host {
				return nil, fmt.Errorf("--host-port %q conflicts with another mapping for sandbox %s port %d", value, mapping.Protocol, mapping.Guest)
			}
			continue
		}
		seen[key] = mapping.Host
		result = append(result, mapping)
	}
	return result, nil
}
