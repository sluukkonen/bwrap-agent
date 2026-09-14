package app

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func writeEtcFixture(t *testing.T, root, relative string) string {
	t.Helper()
	path := filepath.Join(root, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(relative+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPasswdFieldRejectsRecordSeparators(t *testing.T) {
	for _, value := range []string{"", "name:root", "name\nroot", "name\rroot"} {
		if got := passwdField(value, "agent"); got != "agent" {
			t.Errorf("passwdField(%q) = %q", value, got)
		}
	}
	if got := passwdField("developer", "agent"); got != "developer" {
		t.Errorf("valid passwd field = %q", got)
	}
}

func TestGeneratedAccountUsesSafeReachableHomeAlias(t *testing.T) {
	for _, separator := range []string{":", "\n", "\r"} {
		state := filepath.Join("/tmp", "state"+separator+"root")
		passwd, _ := generatedAccountFiles(state, false)
		if !strings.Contains(string(passwd), ":"+sandboxPasswdHome+":/bin/sh") {
			t.Errorf("generated passwd for state %q = %q", state, passwd)
		}
		if home, aliased := accountHome(state); home != sandboxPasswdHome || !aliased {
			t.Errorf("accountHome(%q) = %q, %v", state, home, aliased)
		}
	}
	if home, aliased := accountHome("/tmp/ordinary-state"); home != "/tmp/ordinary-state/home" || aliased {
		t.Fatalf("ordinary account home = %q, %v", home, aliased)
	}
}

func TestSystemEtcUsesExplicitAllowlist(t *testing.T) {
	root := t.TempDir()
	loader := writeEtcFixture(t, root, "ld.so.cache")
	secret := writeEtcFixture(t, root, "shadow")
	machineID := writeEtcFixture(t, root, "machine-id")
	containersConfig := writeEtcFixture(t, root, "containers/containers.conf")
	registryConfig := writeEtcFixture(t, root, "containers/registries.conf")
	mavenConfig := writeEtcFixture(t, root, "m2.conf")
	maven4Config := writeEtcFixture(t, root, "m24.conf")
	maven3Settings := writeEtcFixture(t, root, "maven/settings.xml")
	maven3Logging := writeEtcFixture(t, root, "maven/logging/simplelogger.properties")
	maven4Settings := writeEtcFixture(t, root, "maven4/settings.xml")
	javaConfig := writeEtcFixture(t, root, "java-17-openjdk/security/java.security")
	unrelatedJava := writeEtcFixture(t, root, "java-vendor/private.conf")
	writeEtcFixture(t, root, "ssh/ssh_config")
	writeEtcFixture(t, root, "gitconfig")

	mounts := mountBuilder{made: map[string]bool{"/etc": true}}
	if err := mountSystemEtc(&mounts, root, false); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(mounts.finish(), "\x00")
	if !strings.Contains(joined, "--ro-bind\x00"+loader+"\x00/etc/ld.so.cache") {
		t.Fatalf("portable loader configuration missing: %#v", mounts.finish())
	}
	for source, destination := range map[string]string{
		filepath.Dir(filepath.Dir(javaConfig)): "/etc/java-17-openjdk",
		mavenConfig:                            "/etc/m2.conf",
		maven4Config:                           "/etc/m24.conf",
		maven3Settings:                         "/etc/maven/settings.xml",
		filepath.Dir(maven3Logging):            "/etc/maven/logging",
	} {
		if !strings.Contains(joined, "--ro-bind\x00"+source+"\x00"+destination) {
			t.Errorf("portable Java/Maven configuration missing for %s: %#v", destination, mounts.finish())
		}
	}
	for _, excluded := range []string{secret, machineID, containersConfig, registryConfig, maven4Settings, unrelatedJava, filepath.Join(root, "ssh"), filepath.Join(root, "gitconfig")} {
		if strings.Contains(joined, excluded) {
			t.Errorf("non-allowlisted path exposed: %s", excluded)
		}
	}
	if strings.Contains(joined, "--ro-bind\x00"+root+"\x00/etc") {
		t.Fatalf("source /etc root was mounted: %#v", mounts.finish())
	}

	podmanMounts := mountBuilder{made: map[string]bool{"/etc": true}}
	if err := mountSystemEtc(&podmanMounts, root, true); err != nil {
		t.Fatal(err)
	}
	podmanJoined := strings.Join(podmanMounts.finish(), "\x00")
	if !strings.Contains(podmanJoined, "--ro-bind\x00"+registryConfig+"\x00/etc/containers/registries.conf") {
		t.Fatalf("Podman registry policy missing: %#v", podmanMounts.finish())
	}
	if strings.Contains(podmanJoined, containersConfig) {
		t.Fatalf("host containers.conf was exposed: %#v", podmanMounts.finish())
	}
}

func TestSystemEtcRejectsSpecialAllowlistedPath(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ld.so.cache")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	mounts := mountBuilder{made: map[string]bool{"/etc": true}}
	if err := mountSystemEtc(&mounts, root, false); err == nil || !strings.Contains(err.Error(), "not a regular file or directory") {
		t.Fatalf("special allowlisted path was accepted: %v", err)
	}
}

func TestSystemEtcPreservesAllowlistedSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("../usr/lib/os-release", filepath.Join(root, "os-release")); err != nil {
		t.Fatal(err)
	}
	mounts := mountBuilder{made: map[string]bool{"/etc": true}}
	if err := mountSystemEtc(&mounts, root, false); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(mounts.finish(), "\x00")
	if !strings.Contains(joined, "--symlink\x00../usr/lib/os-release\x00/etc/os-release") {
		t.Fatalf("allowlisted symlink was not preserved: %#v", mounts.finish())
	}
}

func TestSystemEtcIncludesArchExtractedCATrust(t *testing.T) {
	root := t.TempDir()
	trust := writeEtcFixture(t, root, "ca-certificates/extracted/tls-ca-bundle.pem")
	if err := os.MkdirAll(filepath.Join(root, "ssl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/ca-certificates/extracted/tls-ca-bundle.pem", filepath.Join(root, "ssl", "cert.pem")); err != nil {
		t.Fatal(err)
	}
	mounts := mountBuilder{made: map[string]bool{"/etc": true}}
	if err := mountSystemEtc(&mounts, root, false); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(mounts.finish(), "\x00")
	if !strings.Contains(joined, "--ro-bind\x00"+filepath.Dir(trust)+"\x00/etc/ca-certificates/extracted") {
		t.Fatalf("Arch extracted CA trust was not mounted: %#v", mounts.finish())
	}
	if !strings.Contains(joined, "--symlink\x00/etc/ca-certificates/extracted/tls-ca-bundle.pem\x00/etc/ssl/cert.pem") {
		t.Fatalf("Arch CA bundle symlink was not preserved: %#v", mounts.finish())
	}
}

func TestSystemEtcIncludesOpenSUSEExternalCATrust(t *testing.T) {
	root := t.TempDir()
	trust := writeEtcFixture(t, root, "var/lib/ca-certificates/ca-bundle.pem")
	unrelated := writeEtcFixture(t, root, "var/lib/private/secret")
	etcRoot := filepath.Join(root, "etc")
	if err := os.MkdirAll(filepath.Join(etcRoot, "ssl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/lib/ca-certificates", filepath.Join(etcRoot, "ssl", "certs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/lib/ca-certificates/ca-bundle.pem", filepath.Join(etcRoot, "ssl", "ca-bundle.pem")); err != nil {
		t.Fatal(err)
	}
	mounts := mountBuilder{made: map[string]bool{"/etc": true, "/var": true}}
	if err := mountSystemEtc(&mounts, etcRoot, false); err != nil {
		t.Fatal(err)
	}
	if err := mountSystemEtcExternalPaths(&mounts, root); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(mounts.finish(), "\x00")
	if !strings.Contains(joined, "--ro-bind\x00"+filepath.Dir(trust)+"\x00/var/lib/ca-certificates") {
		t.Fatalf("openSUSE CA trust was not mounted: %#v", mounts.finish())
	}
	if strings.Contains(joined, unrelated) || strings.Contains(joined, "--ro-bind\x00"+filepath.Join(root, "var")+"\x00/var") {
		t.Fatalf("external CA trust exposed unrelated /var data: %#v", mounts.finish())
	}
	for _, symlink := range []string{
		"--symlink\x00/var/lib/ca-certificates\x00/etc/ssl/certs",
		"--symlink\x00/var/lib/ca-certificates/ca-bundle.pem\x00/etc/ssl/ca-bundle.pem",
	} {
		if !strings.Contains(joined, symlink) {
			t.Errorf("openSUSE CA symlink missing: %#v", mounts.finish())
		}
	}
}

func TestSystemEtcCreatesParentForNestedSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "maven"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/share/maven/conf/settings.xml", filepath.Join(root, "maven", "settings.xml")); err != nil {
		t.Fatal(err)
	}
	mounts := mountBuilder{made: map[string]bool{"/etc": true}}
	if err := mountSystemEtc(&mounts, root, false); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(mounts.finish(), "\x00")
	if !strings.Contains(joined, "--dir\x00/etc/maven") {
		t.Fatalf("nested symlink parent was not created: %#v", mounts.finish())
	}
	if !strings.Contains(joined, "--symlink\x00/usr/share/maven/conf/settings.xml\x00/etc/maven/settings.xml") {
		t.Fatalf("nested allowlisted symlink was not preserved: %#v", mounts.finish())
	}
}

func TestGeneratedEtcIsMinimalAndNetworkSpecific(t *testing.T) {
	state := t.TempDir()
	files, err := prepareGeneratedEtc(state, t.TempDir(), "private", false, "agent-private-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("private generated files = %#v", files)
	}
	byDestination := make(map[string]string, len(files))
	for _, file := range files {
		content, err := os.ReadFile(file.Source)
		if err != nil {
			t.Fatal(err)
		}
		byDestination[file.Destination] = string(content)
	}
	passwd := byDestination["/etc/passwd"]
	if !strings.Contains(passwd, "root:x:0:0:") || !strings.Contains(passwd, "nobody:x:65534:65534:") || strings.Contains(passwd, "daemon:") {
		t.Fatalf("generated passwd is not minimal: %q", passwd)
	}
	if os.Getuid() != 0 && !strings.Contains(passwd, ":x:"+strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid())+":") {
		t.Fatalf("generated passwd omitted invoking account: %q", passwd)
	}
	if got := byDestination["/etc/nsswitch.conf"]; !strings.Contains(got, "hosts: files dns") || strings.Contains(got, "systemd") || strings.Contains(got, "sss") {
		t.Fatalf("generated NSS policy = %q", got)
	}
	if got := byDestination["/etc/resolv.conf"]; !strings.Contains(got, "nameserver 127.0.0.1") || !strings.Contains(got, "allowed names resolve through the private DNS proxy") {
		t.Fatalf("private resolver = %q", got)
	}
	if got := byDestination["/etc/hosts"]; !strings.Contains(got, "127.0.0.1 agent-private-test") || !strings.Contains(got, "::1 agent-private-test") {
		t.Fatalf("private hosts = %q", got)
	}

	hostFiles, err := prepareGeneratedEtc(t.TempDir(), t.TempDir(), "host", false, "agent-host-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(hostFiles) != 4 {
		t.Fatalf("host mode generated network files: %#v", hostFiles)
	}
	hosts, err := os.ReadFile(hostFiles[3].Source)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hosts), "agent-host-test") {
		t.Fatalf("host-mode hosts omitted sandbox hostname: %q", hosts)
	}
	noneFiles, err := prepareGeneratedEtc(t.TempDir(), t.TempDir(), "none", false, "agent-none-test")
	if err != nil {
		t.Fatal(err)
	}
	noneResolver, err := os.ReadFile(noneFiles[4].Source)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(noneResolver), "nameserver 127.0.0.1") || !strings.Contains(string(noneResolver), "attempts:1 timeout:1") {
		t.Fatalf("none-mode resolver = %q", noneResolver)
	}

	podmanState := t.TempDir()
	podmanFiles, err := prepareGeneratedEtc(podmanState, t.TempDir(), "host", true, "agent-podman-test")
	if err != nil {
		t.Fatal(err)
	}
	podmanPasswd, err := os.ReadFile(podmanFiles[0].Source)
	if err != nil {
		t.Fatal(err)
	}
	if len(podmanFiles) != 4 || !strings.Contains(string(podmanPasswd), "root:x:0:0:root:"+filepath.Join(podmanState, "home")+":/bin/sh") {
		t.Fatalf("Podman root home does not use instance state: %q", podmanPasswd)
	}
}

func TestGeneratedHostsPreservesHostEntries(t *testing.T) {
	hostsPath := writeEtcFixture(t, t.TempDir(), "hosts")
	content, err := generatedHosts(hostsPath, "host", "agent-hostname")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "hosts\n") || !strings.Contains(string(content), "127.0.0.1 agent-hostname\n") || !strings.Contains(string(content), "::1 agent-hostname\n") {
		t.Fatalf("generated host entries = %q", content)
	}
}
