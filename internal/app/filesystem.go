package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type filesystemInputs struct {
	storageConfig     string
	containersConfig  string
	home              homeLayout
	bwrap             string
	podman            bool
	usePTY            bool
	generated         string
	generatedEtc      []injectedFile
	passwdHomeAliased bool
	gitMounts         []resourceMount
	agentMounts       []resourceMount
	podmanMount       resourceMount
	commandMounts     []resourceMount
}

type filesystemLayout struct {
	args               []string
	protectedPaths     []string
	launcherExecutable string
}

// buildFilesystemLayout assembles mounts in precedence order. The caller owns
// the generated directory and must clear it if plan construction fails.
// Options and identity must already be normalized and validated by buildPlan.
func buildFilesystemLayout(opts Options, identity instanceIdentity, input filesystemInputs) (filesystemLayout, error) {
	project := identity.Project
	bwrap := []string{input.bwrap, "--unshare-ipc", "--unshare-pid", "--unshare-uts", "--unshare-cgroup-try", "--die-with-parent"}
	if !input.podman {
		bwrap = append(bwrap, "--unshare-user", "--disable-userns")
	}
	if !input.usePTY {
		bwrap = append(bwrap, "--new-session")
	}
	bwrap = append(bwrap, "--tmpfs", "/", "--dir", "/var", "--dir", "/run", "--proc", "/proc", "--dev", "/dev", "--dir", "/dev/net", "--tmpfs", "/tmp", "--tmpfs", "/var/tmp", "--dir", "/run/bwrap-agent")
	mounts := mountBuilder{args: bwrap, made: map[string]bool{"/dev": true, "/dev/net": true, "/tmp": true, "/var": true, "/var/tmp": true, "/run": true, "/run/bwrap-agent": true}}
	if input.podman {
		mounts.operation("--perms", "0700", "--tmpfs", sandboxRuntimeDirectory)
		mounts.made[sandboxRuntimeDirectory] = true
	}
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		mounts.mount("--dev-bind", "/dev/net/tun", "/dev/net/tun")
	}
	if _, err := os.Stat("/usr"); err == nil {
		mounts.mount("--ro-bind", "/usr", "/usr")
	}
	mounts.operation("--dir", "/etc")
	mounts.made["/etc"] = true
	if err := mountSystemEtc(&mounts, "/etc", input.podman); err != nil {
		return filesystemLayout{}, err
	}
	if err := mountSystemEtcExternalPaths(&mounts, "/"); err != nil {
		return filesystemLayout{}, err
	}
	if opts.Network == "host" {
		for _, path := range []string{"/etc/resolv.conf"} {
			if err := mountEtcPath(&mounts, "/etc", strings.TrimPrefix(path, "/etc/")); err != nil {
				return filesystemLayout{}, err
			}
			if err := mountExternalEtcLinkTarget(&mounts, path); err != nil {
				return filesystemLayout{}, err
			}
		}
	}
	for _, file := range input.generatedEtc {
		mounts.mount("--ro-bind", file.Source, file.Destination)
	}
	for _, legacy := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		info, err := os.Lstat(legacy)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(legacy)
			if err != nil {
				return filesystemLayout{}, err
			}
			mounts.operation("--symlink", target, legacy)
		} else {
			mounts.mount("--ro-bind", legacy, legacy)
		}
	}
	if _, err := os.Stat("/sys"); err == nil {
		option := "--ro-bind"
		if input.podman {
			// Privileged rootless containers expect to reuse the outer user
			// namespace's sysfs mount. The agent runs in a less-privileged child
			// user namespace and cannot exercise the outer namespace's sysfs
			// capabilities even though this bind remains writable.
			option = "--bind"
		}
		mounts.mount(option, "/sys", "/sys")
	}
	mounts.mount("--bind", input.home.backingHome(), input.home.canonicalHome)
	if input.home.home != input.home.canonicalHome {
		mounts.parentDirs(input.home.home)
		mounts.operation("--symlink", input.home.canonicalHome, input.home.home)
	}
	for _, path := range []string{project, identity.GitCommon} {
		if path != "" {
			if err := input.home.validateMount(path, path); err != nil {
				return filesystemLayout{}, err
			}
		}
	}
	switch opts.WorkspaceMode {
	case "write-through":
		mounts.mount("--bind", project, project)
		if identity.GitCommon != "" {
			mounts.mount("--bind", identity.GitCommon, identity.GitCommon)
		}
	case "copy-on-write":
		for _, source := range identity.ROBind {
			if pathsOverlap(source, project) || identity.GitCommon != "" && pathsOverlap(source, identity.GitCommon) {
				return filesystemLayout{}, fmt.Errorf("read-only bind %s overlaps a copy-on-write workspace hierarchy", source)
			}
		}
		mounts.parentDirs(project)
		mounts.operation("--overlay-src", project, "--tmp-overlay", project)
		if identity.GitCommon != "" {
			mounts.parentDirs(identity.GitCommon)
			mounts.operation("--overlay-src", identity.GitCommon, "--tmp-overlay", identity.GitCommon)
		}
	case "read-only":
		mounts.mount("--ro-bind", project, project)
		if identity.GitCommon != "" {
			mounts.mount("--ro-bind", identity.GitCommon, identity.GitCommon)
		}
	}
	if input.passwdHomeAliased {
		mounts.operation("--symlink", input.home.home, sandboxPasswdHome)
	}
	for _, source := range identity.ROBind {
		if err := input.home.validateMount(source, source); err != nil {
			return filesystemLayout{}, err
		}
		mounts.mount("--ro-bind", source, source)
	}
	for _, source := range identity.RWBind {
		if err := input.home.validateMount(source, source); err != nil {
			return filesystemLayout{}, err
		}
		mounts.mount("--bind", source, source)
	}
	controlMounts, protectedPaths, err := prepareControlFileProtection(opts, identity)
	if err != nil {
		return filesystemLayout{}, err
	}
	for _, bind := range controlMounts {
		mounts.mount(bind.option, bind.source, bind.destination)
	}
	for _, bind := range input.gitMounts {
		if err := input.home.mountResource(&mounts, bind); err != nil {
			return filesystemLayout{}, err
		}
	}
	for _, bind := range input.agentMounts {
		if err := input.home.mountResource(&mounts, bind); err != nil {
			return filesystemLayout{}, err
		}
	}
	if input.podman {
		if err := input.home.mountResource(&mounts, input.podmanMount); err != nil {
			return filesystemLayout{}, err
		}
		mounts.mount("--ro-bind", input.storageConfig, filepath.Join(sandboxPodmanConfigDirectory, "storage.conf"))
		mounts.mount("--ro-bind", input.containersConfig, filepath.Join(sandboxPodmanConfigDirectory, "containers.conf"))
	}
	self, err := os.Executable()
	if err != nil {
		return filesystemLayout{}, err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return filesystemLayout{}, err
	}
	initSource, err := copyPrivateExecutable(input.generated, "init", self)
	if err != nil {
		return filesystemLayout{}, err
	}
	mounts.mount("--ro-bind", initSource, "/run/bwrap-agent/init")
	for _, bind := range input.commandMounts {
		if bind.Executable && input.podman {
			info, err := os.Stat(bind.Source)
			if err != nil {
				return filesystemLayout{}, err
			}
			if info.Mode().IsRegular() {
				source, err := copyPrivateExecutable(input.generated, strings.TrimPrefix(bind.Destination, "/run/bwrap-agent/"), bind.Source)
				if err != nil {
					return filesystemLayout{}, err
				}
				mounts.mount("--ro-bind", source, bind.Destination)
				continue
			}
		}
		mounts.mount("--ro-bind", bind.Source, bind.Destination)
	}
	return filesystemLayout{args: mounts.finish(), protectedPaths: protectedPaths, launcherExecutable: self}, nil
}

type mountBuilder struct {
	args []string
	dirs []string
	ops  []string
	made map[string]bool
}

func (m *mountBuilder) parentDirs(path string) {
	var parents []string
	for parent := filepath.Dir(path); parent != "/" && parent != "."; parent = filepath.Dir(parent) {
		parents = append(parents, parent)
	}
	for index := len(parents) - 1; index >= 0; index-- {
		if !m.made[parents[index]] {
			m.dirs = append(m.dirs, "--dir", parents[index])
			m.made[parents[index]] = true
		}
	}
}

func (m *mountBuilder) mount(option, source, destination string) {
	m.parentDirs(destination)
	m.ops = append(m.ops, option, source, destination)
}

func (m *mountBuilder) operation(arguments ...string) {
	m.ops = append(m.ops, arguments...)
}

func (m *mountBuilder) finish() []string {
	result := append([]string(nil), m.args...)
	result = append(result, m.dirs...)
	return append(result, m.ops...)
}

// mountExternalEtcLinkTarget preserves the named destination of a resolver or
// hosts-file symlink. Bubblewrap gives paths such as /run and /var private
// filesystems, so their host-side symlink chains are not otherwise available.
func mountExternalEtcLinkTarget(mounts *mountBuilder, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	linkTarget, err := os.Readlink(path)
	if err != nil {
		return fmt.Errorf("read %s symlink: %w", path, err)
	}
	namedTarget := linkTarget
	if !filepath.IsAbs(namedTarget) {
		namedTarget = filepath.Join(filepath.Dir(path), namedTarget)
	}
	namedTarget = filepath.Clean(namedTarget)
	target, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve %s: %w", path, err)
	}
	targetInfo, err := os.Stat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat resolved %s: %w", path, err)
	}
	if !targetInfo.Mode().IsRegular() {
		return fmt.Errorf("resolved %s is not a regular file: %s", path, target)
	}
	mounts.mount("--ro-bind", target, namedTarget)
	return nil
}
