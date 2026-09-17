package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var terminalEnvironment = map[string]bool{
	"TERM": true, "TERMCAP": true, "TERMINFO": true, "TERMINFO_DIRS": true,
	"COLORTERM": true, "COLORFGBG": true, "TERM_PROGRAM": true,
	"TERM_PROGRAM_VERSION": true, "VTE_VERSION": true, "KONSOLE_VERSION": true,
	"KITTY_WINDOW_ID": true, "KITTY_PID": true, "WT_SESSION": true, "TMUX": true,
	"TMUX_PANE": true, "STY": true, "WINDOW": true, "XTERM_VERSION": true,
	"LINES": true, "COLUMNS": true, "NO_COLOR": true, "FORCE_COLOR": true,
	"CLICOLOR": true, "CLICOLOR_FORCE": true, "NCURSES_NO_UTF8_ACS": true,
	"ESCDELAY": true, "OPENCODE_TERMINAL": true,
}

func safeName(value string) (string, error) {
	if strings.HasPrefix(value, deletionTombstonePrefix) {
		return "", fmt.Errorf("instance names beginning with %q are reserved", deletionTombstonePrefix)
	}
	if value == "" || value == "." || value == ".." || len(value) > 80 {
		return "", errors.New("instance name must be 1-80 characters from [A-Za-z0-9_.-]")
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_.-", char)) {
			return "", errors.New("instance name must be 1-80 characters from [A-Za-z0-9_.-]")
		}
	}
	return value, nil
}

func projectName(project string, reserved int) string {
	var label strings.Builder
	for _, char := range filepath.Base(project) {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_.-", char) {
			label.WriteRune(char)
		} else {
			label.WriteByte('-')
		}
	}
	clean := strings.Trim(label.String(), "._-")
	if clean == "" {
		clean = "project"
	}
	limit := 80 - reserved
	if len(clean) > limit {
		clean = clean[:limit]
	}
	return clean
}

func defaultName(project string) string {
	return projectName(project, 0)
}

func collisionName(project string) string {
	digest := sha256.Sum256([]byte(project))
	suffix := "-" + hex.EncodeToString(digest[:])[:10]
	return projectName(project, len(suffix)) + suffix
}

type instanceIdentity struct {
	Project   string
	Instance  string
	State     string
	Root      string
	GitCommon string
	Git       gitMetadata
	ROBind    []string
	RWBind    []string
}

func resolveProjectDirectory(path string) (string, error) {
	project, err := resolveExisting(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(project)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("project is not a directory: %s", project)
	}
	return project, nil
}

func expandUser(path string) (string, error) {
	if path == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	if strings.HasPrefix(path, "~") {
		name, rest, found := strings.Cut(path[1:], "/")
		if !found {
			rest = ""
		}
		account, err := user.Lookup(name)
		if err != nil {
			return "", err
		}
		return filepath.Join(account.HomeDir, rest), nil
	}
	return path, nil
}

func resolveExisting(path string) (string, error) {
	expanded, err := expandUser(path)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

func stateBase() (string, error) {
	if configured := os.Getenv("BWRAP_AGENT_STATE_HOME"); configured != "" {
		return expandUser(configured)
	}
	if configured := os.Getenv("XDG_STATE_HOME"); configured != "" {
		expanded, err := expandUser(configured)
		return filepath.Join(expanded, "bwrap-agent"), err
	}
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "bwrap-agent"), err
}

func resolveState(path string) (string, error) {
	expanded, err := expandUser(path)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	current := absolute
	var missing []string
	for {
		resolved, evalErr := filepath.EvalSymlinks(current)
		if evalErr == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		}
		if !errors.Is(evalErr, os.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return absolute, nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func freePort(protocol string) (int, error) {
	if protocol == "udp" {
		listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
		if err != nil {
			return 0, err
		}
		defer listener.Close()
		return listener.LocalAddr().(*net.UDPAddr).Port, nil
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func parsePort(value string) (PortMapping, error) {
	main, protocol, slash := value, "tcp", false
	if before, after, found := strings.Cut(value, "/"); found {
		main, protocol, slash = before, after, true
	}
	_ = slash
	if protocol != "tcp" && protocol != "udp" {
		return PortMapping{}, fmt.Errorf("unsupported protocol in --publish %q", value)
	}
	fields := strings.Split(main, ":")
	var hostText, guestText string
	switch len(fields) {
	case 1:
		hostText, guestText = fields[0], fields[0]
	case 2:
		hostText, guestText = fields[0], fields[1]
	default:
		return PortMapping{}, fmt.Errorf("invalid --publish value %q", value)
	}
	host, hostErr := strconv.Atoi(hostText)
	guest, guestErr := strconv.Atoi(guestText)
	if hostErr != nil || guestErr != nil {
		return PortMapping{}, fmt.Errorf("ports must be integers in --publish %q", value)
	}
	if guest < 1 || guest > 65535 || host < 0 || host > 65535 {
		return PortMapping{}, fmt.Errorf("port out of range in --publish %q", value)
	}
	if host == 0 {
		allocated, err := freePort(protocol)
		if err != nil {
			return PortMapping{}, err
		}
		host = allocated
	}
	return PortMapping{Protocol: protocol, Host: host, Guest: guest}, nil
}

func requireProgram(name string) (string, error) {
	found, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("required program not found in PATH: %s", name)
	}
	absolute, err := filepath.Abs(found)
	if err != nil {
		return "", err
	}
	return absolute, nil
}

func requireSystemShell() (string, error) {
	const path = "/bin/sh"
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("required system shell unavailable at %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("required system shell is not an executable regular file: %s", path)
	}
	if err := unix.Access(path, unix.X_OK); err != nil {
		return "", fmt.Errorf("required system shell is not executable at %s: %w", path, err)
	}
	return path, nil
}

func resolvePodman(mode string) (string, error) {
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "off":
		return "", nil
	case "auto":
		found, err := exec.LookPath("podman")
		if err != nil {
			return "", nil
		}
		return filepath.Abs(found)
	case "on":
		return requireProgram("podman")
	default:
		return "", fmt.Errorf("invalid Podman mode %q: expected auto, on, or off", mode)
	}
}

func resolveWorkspaceMode(mode string) (string, error) {
	if mode == "" {
		mode = "write-through"
	}
	if mode != "write-through" && mode != "copy-on-write" && mode != "read-only" {
		return "", fmt.Errorf("invalid workspace mode %q: expected write-through, copy-on-write, or read-only", mode)
	}
	return mode, nil
}

func resolveLandlockMode(mode string) (string, error) {
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "required" && mode != "off" {
		return "", fmt.Errorf("invalid Landlock mode %q: expected auto, required, or off", mode)
	}
	return mode, nil
}

func resolveSeccompMode(mode string) (string, error) {
	if mode == "" {
		mode = "auto"
	}
	if mode != "auto" && mode != "required" && mode != "off" {
		return "", fmt.Errorf("invalid seccomp mode %q: expected auto, required, or off", mode)
	}
	return mode, nil
}

func envValue(source []string, name, fallback string) string {
	for _, assignment := range source {
		key, value, found := strings.Cut(assignment, "=")
		if found && key == name {
			return value
		}
	}
	return fallback
}

func openInjectedFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("injected source is not a regular file: %s", path)
	}
	return os.NewFile(uintptr(fd), path), nil
}

func launchBubblewrap(argv []string) int {
	seccompProfile := ""
	if len(argv) >= 2 && argv[0] == "--seccomp-profile" {
		seccompProfile = argv[1]
		argv = argv[2:]
	}
	separator := -1
	for index, argument := range argv {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(argv) {
		fmt.Fprintln(os.Stderr, "bwrap-agent: internal Bubblewrap launcher received no command")
		return 126
	}
	sources, command := argv[:separator], argv[separator+1:]
	if len(sources) != 0 {
		fmt.Fprintln(os.Stderr, "bwrap-agent: internal Bubblewrap launcher received unexpected file sources")
		return 126
	}
	if seccompProfile != "" {
		filter, err := openSeccompFilter(seccompProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: prepare seccomp profile: %v\n", err)
			return 126
		}
		defer filter.Close()
		sourceFD := int(filter.Fd())
		if sourceFD == 3 {
			_, err = unix.FcntlInt(uintptr(sourceFD), unix.F_SETFD, 0)
		} else {
			err = unix.Dup3(sourceFD, 3, 0)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: prepare seccomp descriptor: %v\n", err)
			return 126
		}
	}
	if err := unix.Exec(command[0], command, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: launch Bubblewrap: %v\n", err)
		return launchErrorCode(err)
	}
	return 0
}

func BuildPlan(opts Options) (LaunchPlan, error) {
	identity, lock, err := resolveAndLockInstance(opts)
	if err != nil {
		return LaunchPlan{}, err
	}
	defer lock.Close()
	return buildPlan(opts, identity)
}

func buildPlan(opts Options, identity instanceIdentity) (result LaunchPlan, resultErr error) {
	hostEnv := os.Environ()
	generated, err := preparePrivateFiles(identity)
	if err != nil {
		return LaunchPlan{}, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, clearPrivateFiles(generated))
		}
	}()
	project, instance, state := identity.Project, identity.Instance, identity.State
	layout, err := resolveHomeLayout(identity)
	if err != nil {
		return LaunchPlan{}, err
	}
	if err := layout.prepare(); err != nil {
		return LaunchPlan{}, fmt.Errorf("prepare sandbox home: %w", err)
	}
	workspaceMode, err := resolveWorkspaceMode(opts.WorkspaceMode)
	if err != nil {
		return LaunchPlan{}, err
	}
	opts.WorkspaceMode = workspaceMode
	landlockMode, err := resolveLandlockMode(opts.Landlock)
	if err != nil {
		return LaunchPlan{}, err
	}
	opts.Landlock = landlockMode
	seccompMode, err := resolveSeccompMode(opts.Seccomp)
	if err != nil {
		return LaunchPlan{}, err
	}
	opts.Seccomp = seccompMode
	opts.NetworkAllow, err = normalizeNetworkAllowList(opts.NetworkAllow)
	if err != nil {
		return LaunchPlan{}, fmt.Errorf("network allowlist: %w", err)
	}
	if opts.Network == "host" && len(opts.NetworkAllow) > 0 {
		return LaunchPlan{}, errors.New("--network-allow cannot be used with --network=host")
	}
	bwrapBin, err := requireProgram("bwrap")
	if err != nil {
		return LaunchPlan{}, err
	}
	bwrapInfo, err := inspectBubblewrap(bwrapBin)
	if err != nil {
		return LaunchPlan{}, err
	}
	if workspaceMode == "read-only" || landlockMode == "required" {
		if opts.Podman == "on" {
			if workspaceMode == "read-only" {
				return LaunchPlan{}, errors.New("--podman=on is incompatible with --workspace-mode=read-only")
			}
			return LaunchPlan{}, errors.New("--podman=on is incompatible with --landlock=required")
		}
		if opts.Podman == "" || opts.Podman == "auto" {
			opts.Podman = "off"
		}
	}
	podmanBin, err := resolvePodman(opts.Podman)
	if err != nil {
		return LaunchPlan{}, err
	}
	var storageConfig, containersConfig, hostAccount string
	if podmanBin != "" {
		if err := validatePodmanStorageMounts(layout, identity); err != nil {
			return LaunchPlan{}, err
		}
		hostAccount, err = hostAccountName(hostEnv)
		if err != nil {
			return LaunchPlan{}, err
		}
		storageConfig, err = writeStorageConfig(generated, layout)
		if err != nil {
			return LaunchPlan{}, err
		}
		containersConfig, err = writeContainersConfig(generated, opts.Network == "private", workspaceMode == "copy-on-write")
		if err != nil {
			return LaunchPlan{}, err
		}
	}
	sandboxHostname := "agent-" + truncate(instance, 48)
	_, passwdHomeAliased := accountHome(layout.home)
	generatedEtc, err := prepareGeneratedEtc(layout.home, generated, opts.Network, podmanBin != "", sandboxHostname)
	if err != nil {
		return LaunchPlan{}, fmt.Errorf("prepare generated /etc: %w", err)
	}
	landlockStatus, err := determineLandlockStatus(landlockMode, podmanBin != "", queryLandlockABI)
	if err != nil {
		return LaunchPlan{}, err
	}
	seccompStatus, seccompWarning, err := determineSeccompStatus(seccompMode, podmanBin != "", querySeccompSupport)
	if err != nil {
		return LaunchPlan{}, err
	}
	var warnings []string
	if seccompWarning != "" {
		warnings = append(warnings, seccompWarning)
	}
	var pastaBin, launcherTrampoline string
	if opts.Network == "private" {
		pastaBin, err = requireProgram("pasta")
		if err != nil {
			return LaunchPlan{}, err
		}
		launcherTrampoline, err = requireSystemShell()
		if err != nil {
			return LaunchPlan{}, err
		}
	}
	adapter := detectAgent(opts.Command[0])
	host := hostContext{state: state, hostEnv: hostEnv, sources: newHostSourcePolicy(identity)}
	gitConfig, err := prepareGitConfigMounts(host, !opts.NoGitConfig)
	if err != nil {
		return LaunchPlan{}, err
	}
	var podmanMount resourceMount
	if podmanBin != "" {
		podmanMount, err = preparePodmanConfigMount(host, generated)
		if err != nil {
			return LaunchPlan{}, err
		}
	}
	agent, err := prepareAgent(adapter, host, !opts.NoAgentConfig)
	if err != nil {
		return LaunchPlan{}, err
	}
	var resolveExternal externalCommandResolver
	if adapter != nil {
		resolveExternal = adapter.resolveExternalCommand
	}
	command, err := resolveCommand(host, opts.Command, resolveExternal)
	if err != nil {
		return LaunchPlan{}, err
	}
	ports := make([]PortMapping, 0, len(opts.Publish))
	for _, value := range opts.Publish {
		port, err := parsePort(value)
		if err != nil {
			return LaunchPlan{}, err
		}
		ports = append(ports, port)
	}
	if len(ports) > 0 && opts.Network != "private" {
		return LaunchPlan{}, errors.New("--publish requires --network private")
	}
	hostPorts, err := resolveHostPorts(opts.HostPort, opts.Network, ports)
	if err != nil {
		return LaunchPlan{}, err
	}
	usePTY := opts.TTY == "always" || opts.TTY == "auto" && isTerminal(os.Stdin.Fd()) && isTerminal(os.Stdout.Fd())
	clipboardMode, clipboardConfig, err := prepareClipboard(opts.Clipboard, usePTY, host)
	if err != nil {
		return LaunchPlan{}, err
	}

	accountName, _ := currentAccount()
	input := environmentInputs{
		home: layout, hostEnv: hostEnv, identity: identity, defaultAccount: accountName, hostAccount: hostAccount,
		podman: podmanBin != "", storageConfig: filepath.Join(sandboxPodmanConfigDirectory, "storage.conf"), containersConfig: filepath.Join(sandboxPodmanConfigDirectory, "containers.conf"),
		agent: agent.Environment, command: command.Environment,
	}
	environment, err := buildSandboxEnvironment(opts, input)
	if err != nil {
		return LaunchPlan{}, err
	}
	if landlockStatus.Effective == "enabled" {
		hostWritePaths := append([]string{}, identity.RWBind...)
		if workspaceMode != "read-only" {
			hostWritePaths = append(hostWritePaths, project)
			if identity.GitCommon != "" {
				hostWritePaths = append(hostWritePaths, identity.GitCommon)
			}
		}
		// Bubblewrap creates these paths in the sandbox independently of whether
		// matching paths exist on the host. They are present by the time the
		// sandbox runtime applies Landlock.
		writePaths, err := prepareLandlockWritePaths(hostWritePaths, []string{layout.canonicalHome, "/tmp", "/var/tmp", "/run", "/dev"})
		if err != nil {
			return LaunchPlan{}, fmt.Errorf("prepare Landlock write paths: %w", err)
		}
		encoded, err := json.Marshal(writePaths)
		if err != nil {
			return LaunchPlan{}, err
		}
		environment[internalLandlockEnvironment] = string(encoded)
	}

	filesystem, err := buildFilesystemLayout(opts, identity, filesystemInputs{
		home: layout, bwrap: bwrapBin, podman: podmanBin != "", usePTY: usePTY,
		generated: generated, generatedEtc: generatedEtc, passwdHomeAliased: passwdHomeAliased,
		gitMounts: gitConfig, agentMounts: agent.Mounts, podmanMount: podmanMount,
		commandMounts: command.Mounts, storageConfig: storageConfig, containersConfig: containersConfig,
	})
	if err != nil {
		return LaunchPlan{}, err
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	bwrap := filesystem.args
	if seccompStatus.Effective == "enabled" {
		bwrap = append(bwrap, "--seccomp", "3")
	}
	for _, name := range names {
		bwrap = append(bwrap, "--setenv", name, environment[name])
	}
	// Removing a value from the sandbox map is not enough: outer processes
	// can require or introduce it again before Bubblewrap starts.
	for _, name := range opts.UnsetEnv {
		if _, required := environment[name]; !required {
			bwrap = append(bwrap, "--unsetenv", name)
		}
	}
	if podmanBin != "" {
		// The supervisor uses only our generated configuration. Its CONTAINERS_CONF
		// must not suppress user configuration in the sandbox.
		bwrap = append(bwrap, "--unsetenv", "CONTAINERS_CONF", "--unsetenv", "CONTAINERS_GRAPHROOT", "--unsetenv", "CONTAINERS_RUNROOT")
	}
	initMode := internalInitMode
	if podmanBin != "" {
		initMode = internalPodmanInitMode
	}
	bwrap = append(bwrap, "--hostname", sandboxHostname, "--chdir", project, "/run/bwrap-agent/init", initMode)
	bwrap = append(bwrap, command.Argv...)
	if opts.Network == "none" {
		bwrap = append([]string{bwrap[0], "--unshare-net"}, bwrap[1:]...)
	}

	launchEnv := buildLauncherEnvironment(environment, input)
	proxyPort := 0
	if opts.Network == "private" {
		proxyPort = proxyGuestPort
	}
	launcher := []string{filesystem.launcherExecutable, internalLaunchMode}
	if launcherTrampoline != "" {
		// Fedora's SELinux policy leaves a user-home executable launched directly
		// by pasta in pasta_t, which cannot traverse common state-home labels.
		// A system executable trampoline restores the caller domain before this
		// launcher runs Bubblewrap. Pass the launcher as the
		// shell's quoted $0 so every valid path remains an opaque argument.
		launcher = append([]string{launcherTrampoline, "-c", `exec "$0" "$@"`}, launcher...)
	}
	if seccompStatus.Effective == "enabled" {
		launcher = append(launcher, "--seccomp-profile", seccompStatus.Profile)
	}
	launcher = append(launcher, "--")
	return LaunchPlan{Clipboard: clipboardMode, clipboardBridge: clipboardConfig, Instance: instance, Project: project, State: state, WorkspaceMode: workspaceMode,
		Landlock: landlockStatus, Seccomp: seccompStatus, Bubblewrap: bwrapInfo.BubblewrapStatus,
		NetworkAllow: append([]string{}, opts.NetworkAllow...), ProxyGuestPort: proxyPort,
		Command: command.Argv, outer: outerCommand{podman: podmanBin, pasta: pastaBin},
		Launcher: launcher,
		Bwrap:    bwrap, Ports: ports, HostPorts: hostPorts, LaunchEnv: launchEnv,
		TTY: usePTY, ConfigFiles: opts.ConfigFiles, ProtectedPaths: filesystem.protectedPaths,
		Warnings: warnings}, nil
}

func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func truncate(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
}

func cloneMap(source map[string]string) map[string]string {
	copy := make(map[string]string, len(source))
	for name, value := range source {
		copy[name] = value
	}
	return copy
}
