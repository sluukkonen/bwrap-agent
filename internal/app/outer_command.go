package app

import (
	"errors"
	"fmt"
	"strconv"
)

// outerCommand describes the namespace wrappers without baking runtime ports
// into argument strings. Empty executable paths omit the corresponding wrapper.
type outerCommand struct {
	podman string
	pasta  string
}

func (outer outerCommand) argv(ports []PortMapping, proxyPort int, proxyHostPort, dnsHostPort string) []string {
	var argv []string
	if outer.podman != "" {
		argv = append(argv, outer.podman, "unshare")
	}
	if outer.pasta != "" {
		argv = append(argv, outer.pasta, "--quiet", "--config-net", "--splice-only")
		if outer.podman != "" {
			argv = append(argv, "--netns-only")
		}
		tcpNamespacePorts := fmt.Sprintf("%d:%s,%d:%s", proxyPort, proxyHostPort, dnsGuestPort, dnsHostPort)
		dnsNamespacePort := fmt.Sprintf("%d:%s", dnsGuestPort, dnsHostPort)
		argv = append(argv, "--tcp-ports", "none", "--udp-ports", "none", "--tcp-ns", tcpNamespacePorts, "--udp-ns", dnsNamespacePort)
		for _, port := range ports {
			option := "--tcp-ports"
			if port.Protocol == "udp" {
				option = "--udp-ports"
			}
			argv = append(argv, option, fmt.Sprintf("127.0.0.1/%d:%d", port.Host, port.Guest))
		}
		argv = append(argv, "--")
	}
	return argv
}

func (p LaunchPlan) argv(proxyHostPort, dnsHostPort string) []string {
	outer := p.outer.argv(p.Ports, p.ProxyGuestPort, proxyHostPort, dnsHostPort)
	argv := make([]string, 0, len(outer)+len(p.Launcher)+len(p.Bwrap))
	argv = append(argv, outer...)
	argv = append(argv, p.Launcher...)
	argv = append(argv, p.Bwrap...)
	return argv
}

func (p LaunchPlan) Argv() []string {
	return p.argv(proxyPortPlaceholder, dnsPortPlaceholder)
}

func (p LaunchPlan) runtimeArgv(proxyHostPort, dnsHostPort int) ([]string, error) {
	if p.ProxyGuestPort == 0 {
		return p.Argv(), nil
	}
	if proxyHostPort < 1 || proxyHostPort > 65535 {
		return nil, fmt.Errorf("invalid runtime proxy port %d", proxyHostPort)
	}
	if dnsHostPort < 1 || dnsHostPort > 65535 {
		return nil, fmt.Errorf("invalid runtime DNS port %d", dnsHostPort)
	}
	if p.outer.pasta == "" {
		return nil, errors.New("private network plan is missing a pasta executable")
	}
	return p.argv(strconv.Itoa(proxyHostPort), strconv.Itoa(dnsHostPort)), nil
}
