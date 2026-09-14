package app

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

type injectedFile struct {
	Source      string
	Destination string
}

type generatedEtcFile struct {
	name    string
	content []byte
}

const sandboxPasswdHome = "/run/bwrap-agent/passwd-home"

// portableEtcPaths contains runtime data that host-installed programs commonly
// require. Keep tool preferences, credentials, and host identity out of this
// list; callers can expose additional paths with an explicit read-only bind.
var portableEtcPaths = []string{
	"alternatives",
	"ld.so.cache",
	"ld.so.conf",
	"ld.so.conf.d",
	"ld-musl-x86_64.path",
	"ld-musl-aarch64.path",
	"java",
	"m2.conf",
	"maven/m2.conf",
	"maven/settings.xml",
	"maven/toolchains.xml",
	"maven/logging",
	"maven4/logging",
	"maven4/maven.properties",
	"host.conf",
	"gai.conf",
	"networks",
	"protocols",
	"rpc",
	"services",
	"localtime",
	"timezone",
	"mtab",
	"os-release",
	"lsb-release",
	"alpine-release",
	"arch-release",
	"debian_version",
	"fedora-release",
	"gentoo-release",
	"redhat-release",
	"system-release",
	"SuSE-release",
	"ssl/certs",
	"ssl/cert.pem",
	"ssl/ca-bundle.pem",
	"ssl/openssl.cnf",
	"ca-certificates/extracted",
	"pki/ca-trust",
	"pki/java",
	"pki/tls/certs",
	"pki/tls/openssl.cnf",
	"crypto-policies",
	"gnutls",
}

// Debian and Ubuntu encode the OpenJDK feature version in the /etc directory
// name. Match only that package-owned top-level layout instead of broadening
// the allowlist to unrelated Java configuration.
var portableEtcPatterns = []string{"java-*-openjdk"}

// Some distributions keep public trust data outside /etc and link their
// allowlisted certificate paths to it. These roots contain only system CA
// material and remain much narrower than exposing the host /var hierarchy.
var portableEtcExternalPaths = []string{"var/lib/ca-certificates"}

var podmanEtcPaths = []string{
	"fuse.conf",
	"selinux",
	"pki/rpm-gpg",
	"containers/registries.conf",
	"containers/registries.conf.d",
	"containers/registries.d",
	"containers/policy.json",
}

func passwdField(value, fallback string) string {
	if value == "" || strings.ContainsAny(value, ":\n\r") {
		return fallback
	}
	return value
}

func currentAccount() (name, group string) {
	name, group = "agent", "agent"
	account, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err == nil {
		name = passwdField(account.Username, name)
	}
	primary, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if err == nil {
		group = passwdField(primary.Name, group)
	}
	if os.Getuid() != 0 && (name == "root" || name == "nobody") {
		name = "agent"
	}
	if os.Getgid() != 0 && os.Getgid() != 65534 && (group == "root" || group == "nobody" || group == "nogroup") {
		group = "agent"
	}
	return name, group
}

func generatedAccountFiles(state string, podmanEnabled bool) (passwd, group []byte) {
	name, groupName := currentAccount()
	home, _ := accountHome(state)
	uid, gid := os.Getuid(), os.Getgid()

	rootHome := "/root"
	// The agent is UID 0 in the outer Podman user namespace. Point Java and
	// other runtimes that consult passwd instead of HOME at the managed home.
	if podmanEnabled || uid == 0 {
		rootHome = home
	}
	passwdLines := []string{fmt.Sprintf("root:x:0:0:root:%s:/bin/sh", rootHome)}
	if uid != 0 {
		passwdLines = append(passwdLines, fmt.Sprintf("%s:x:%d:%d::%s:/bin/sh", name, uid, gid, home))
	}
	if uid != 65534 {
		passwdLines = append(passwdLines, "nobody:x:65534:65534:nobody:/:/usr/sbin/nologin")
	}

	groupLines := []string{"root:x:0:"}
	if gid != 0 {
		groupLines = append(groupLines, fmt.Sprintf("%s:x:%d:", groupName, gid))
	}
	if gid != 65534 {
		groupLines = append(groupLines, "nobody:x:65534:")
	}
	return []byte(strings.Join(passwdLines, "\n") + "\n"), []byte(strings.Join(groupLines, "\n") + "\n")
}

func accountHome(state string) (string, bool) {
	home := filepath.Join(state, "home")
	if passwdField(home, "") == "" {
		return sandboxPasswdHome, true
	}
	return home, false
}

func generatedHosts(hostsPath, network, hostname string) ([]byte, error) {
	content := []byte("127.0.0.1 localhost\n::1 localhost\n")
	if network == "host" {
		info, err := os.Stat(hostsPath)
		if err == nil {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("host hosts path is not a regular file: %s", hostsPath)
			}
			content, err = os.ReadFile(hostsPath)
			if err != nil {
				return nil, fmt.Errorf("read host hosts file %s: %w", hostsPath, err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("inspect host hosts file %s: %w", hostsPath, err)
		}
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		content = append(content, '\n')
	}
	return append(content, []byte(fmt.Sprintf("127.0.0.1 %s\n::1 %s\n", hostname, hostname))...), nil
}

func prepareGeneratedEtc(state, generated, network string, podmanEnabled bool, hostname string) ([]injectedFile, error) {
	passwd, group := generatedAccountFiles(state, podmanEnabled)
	hosts, err := generatedHosts("/etc/hosts", network, hostname)
	if err != nil {
		return nil, err
	}
	files := []generatedEtcFile{
		{name: "passwd", content: passwd},
		{name: "group", content: group},
		{name: "nsswitch.conf", content: []byte("passwd: files\ngroup: files\nshadow: files\nhosts: files dns\nnetworks: files dns\nprotocols: files\nservices: files\nethers: files\nrpc: files\n")},
		{name: "hosts", content: hosts},
	}
	if network != "host" {
		resolv := []byte("# Generated by bwrap-agent; this network mode has no host DNS access.\nnameserver 127.0.0.1\noptions attempts:1 timeout:1\n")
		if network == "private" {
			resolv = []byte("# Generated by bwrap-agent; allowed names resolve through the private DNS proxy.\nnameserver 127.0.0.1\noptions attempts:1 timeout:2\n")
		}
		files = append(files, generatedEtcFile{name: "resolv.conf", content: resolv})
	}
	result := make([]injectedFile, 0, len(files))
	for _, file := range files {
		source, err := writeStateFile(generated, filepath.Join("etc", file.name), file.content, 0o444)
		if err != nil {
			return nil, err
		}
		result = append(result, injectedFile{Source: source, Destination: filepath.Join("/etc", file.name)})
	}
	return result, nil
}

func mountEtcPattern(mounts *mountBuilder, sourceRoot, pattern string) error {
	entries, err := os.ReadDir(sourceRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read system directory %s: %w", sourceRoot, err)
	}
	for _, entry := range entries {
		matched, err := filepath.Match(pattern, entry.Name())
		if err != nil {
			return fmt.Errorf("invalid system path pattern %q: %w", pattern, err)
		}
		if matched {
			if err := mountEtcPath(mounts, sourceRoot, entry.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

func mountEtcPath(mounts *mountBuilder, sourceRoot, relative string) error {
	source := filepath.Join(sourceRoot, relative)
	destination := filepath.Join("/etc", relative)
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect system path %s: %w", source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return fmt.Errorf("read system symlink %s: %w", source, err)
		}
		// Recreate the link without mounting an arbitrary resolved target. Links
		// into /usr, /proc, or another allowlisted subtree work naturally.
		mounts.parentDirs(destination)
		mounts.operation("--symlink", target, destination)
		return nil
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("allowlisted system path is not a regular file or directory: %s", source)
	}
	mounts.mount("--ro-bind", source, destination)
	return nil
}

func mountSystemEtc(mounts *mountBuilder, sourceRoot string, podmanEnabled bool) error {
	for _, relative := range portableEtcPaths {
		if err := mountEtcPath(mounts, sourceRoot, relative); err != nil {
			return err
		}
	}
	for _, pattern := range portableEtcPatterns {
		if err := mountEtcPattern(mounts, sourceRoot, pattern); err != nil {
			return err
		}
	}
	if podmanEnabled {
		for _, relative := range podmanEtcPaths {
			if err := mountEtcPath(mounts, sourceRoot, relative); err != nil {
				return err
			}
		}
	}
	return nil
}

func mountSystemEtcExternalPaths(mounts *mountBuilder, sourceRoot string) error {
	for _, relative := range portableEtcExternalPaths {
		source := filepath.Join(sourceRoot, relative)
		destination := filepath.Join("/", relative)
		info, err := os.Stat(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect external system path %s: %w", source, err)
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("allowlisted external system path is not a regular file or directory: %s", source)
		}
		mounts.mount("--ro-bind", source, destination)
	}
	return nil
}
