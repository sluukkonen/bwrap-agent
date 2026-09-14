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

func terminalEnv(source []string) map[string]string {
	selected := map[string]string{}
	for _, assignment := range source {
		name, value, found := strings.Cut(assignment, "=")
		if !found || strings.ContainsRune(name, 0) || strings.ContainsRune(value, 0) {
			continue
		}
		if terminalEnvironment[name] || strings.HasPrefix(name, "LC_") || strings.HasPrefix(name, "OPENTUI_") {
			selected[name] = value
		}
	}
	return selected
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

func resolveCommand(context agentContext, requested []string, adapter *agentAdapter) ([]string, agentSetup, error) {
	found, err := exec.LookPath(requested[0])
	if err != nil {
		return nil, agentSetup{}, fmt.Errorf("command not found: %s", requested[0])
	}
	absolute, err := filepath.Abs(found)
	if err != nil {
		return nil, agentSetup{}, err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, agentSetup{}, err
	}
	for _, prefix := range []string{"/usr/", "/bin/", "/sbin/", "/lib/", "/lib64/"} {
		if strings.HasPrefix(resolved, prefix) {
			return append([]string(nil), requested...), agentSetup{}, nil
		}
	}
	if adapter != nil && adapter.resolveExternalCommand != nil {
		return adapter.resolveExternalCommand(context, requested, resolved)
	}
	command := append([]string{"/run/bwrap-agent/command"}, requested[1:]...)
	return command, agentSetup{Mounts: []agentMount{{Source: resolved, Destination: "/run/bwrap-agent/command", Executable: true}}}, nil
}

type mountBuilder struct {
	args []string
	dirs []string
	ops  []string
	made map[string]bool
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
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: locate sandbox init: %v\n", err)
		return 126
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: resolve sandbox init: %v\n", err)
		return 126
	}
	paths := append([]string{self}, sources...)
	files := make([]*os.File, 0, len(paths)+1)
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	for _, path := range paths {
		file, err := openInjectedFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: open injected source %s: %v\n", path, err)
			return 126
		}
		files = append(files, file)
	}
	if seccompProfile != "" {
		filter, err := openSeccompFilter(seccompProfile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: prepare seccomp profile: %v\n", err)
			return 126
		}
		files = append(files, filter)
	}
	for index, file := range files {
		sourceFD, targetFD := int(file.Fd()), 3+index
		if sourceFD == targetFD {
			if _, err := unix.FcntlInt(uintptr(sourceFD), unix.F_SETFD, 0); err != nil {
				fmt.Fprintf(os.Stderr, "bwrap-agent: preserve executable descriptor: %v\n", err)
				return 126
			}
		} else if err := unix.Dup3(sourceFD, targetFD, 0); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: prepare executable descriptor: %v\n", err)
			return 126
		}
	}
	if err := unix.Exec(command[0], command, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: launch Bubblewrap: %v\n", err)
		return launchErrorCode(err)
	}
	return 0
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

func BuildPlan(opts Options) (LaunchPlan, error) {
	identity, lock, err := resolveAndLockInstance(opts)
	if err != nil {
		return LaunchPlan{}, err
	}
	defer lock.Close()
	return buildPlan(opts, identity)
}

func buildPlan(opts Options, identity instanceIdentity) (LaunchPlan, error) {
	project, instance, state := identity.Project, identity.Instance, identity.State
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
	if err := secureMkdir(state, 0o700); err != nil {
		return LaunchPlan{}, err
	}
	for _, child := range []string{"home", "run", "tmp", "podman"} {
		if _, err := ensureStateDirectory(state, child, 0o700); err != nil {
			return LaunchPlan{}, err
		}
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
		hostAccount, err = hostAccountName(os.Environ())
		if err != nil {
			return LaunchPlan{}, err
		}
		storageConfig, err = writeStorageConfig(state)
		if err != nil {
			return LaunchPlan{}, err
		}
		containersConfig, err = writeContainersConfig(state, opts.Network == "private", workspaceMode == "copy-on-write")
		if err != nil {
			return LaunchPlan{}, err
		}
	}
	sandboxHostname := "agent-" + truncate(instance, 48)
	_, passwdHomeAliased := accountHome(state)
	generatedEtc, err := prepareGeneratedEtc(state, opts.Network, podmanBin != "", sandboxHostname)
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
	agentContext := agentContext{
		state: state, project: project, gitCommon: identity.GitCommon,
		rwBind: identity.RWBind, hostEnv: os.Environ(), config: !opts.NoAgentConfig,
	}
	var podmanMount agentMount
	if podmanBin != "" {
		podmanMount, err = preparePodmanConfigMount(agentContext)
		if err != nil {
			return LaunchPlan{}, err
		}
		// containers/image versions that ignore XDG_CONFIG_HOME look here for
		// registries.conf, policy.json, and related user configuration.
		if err := validateAgentStateMountpoint(state, filepath.Join(state, "home", ".config", "containers"), podmanMount.Source); err != nil {
			return LaunchPlan{}, err
		}
	}
	agent, err := prepareAgent(adapter, agentContext)
	if err != nil {
		return LaunchPlan{}, err
	}
	command, commandSetup, err := resolveCommand(agentContext, opts.Command, adapter)
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
	usePTY := opts.TTY == "always" || opts.TTY == "auto" && isTerminal(os.Stdin.Fd()) && isTerminal(os.Stdout.Fd())
	clipboardMode, clipboardConfig, err := prepareClipboard(opts.Clipboard, usePTY, agentContext)
	if err != nil {
		return LaunchPlan{}, err
	}

	accountName, _ := currentAccount()
	environment := map[string]string{
		"HOME": filepath.Join(state, "home"), "USER": envValue(os.Environ(), "USER", accountName),
		"LOGNAME":         envValue(os.Environ(), "LOGNAME", envValue(os.Environ(), "USER", accountName)),
		"PATH":            "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"XDG_CONFIG_HOME": filepath.Join(state, "config"), "XDG_CACHE_HOME": filepath.Join(state, "home", ".cache"),
		"XDG_DATA_HOME": filepath.Join(state, "data"), "XDG_STATE_HOME": filepath.Join(state, "home", ".local", "state"),
		"XDG_RUNTIME_DIR": sandboxRuntimeDirectory, "TMPDIR": filepath.Join(state, "tmp"),
		"BWRAP_AGENT_INSTANCE": instance,
	}
	if opts.Network == "private" {
		proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyGuestPort)
		environment["HTTP_PROXY"], environment["http_proxy"] = proxyURL, proxyURL
		environment["HTTPS_PROXY"], environment["https_proxy"] = proxyURL, proxyURL
		environment["NO_PROXY"], environment["no_proxy"] = "localhost,127.0.0.1,::1", "localhost,127.0.0.1,::1"
	}
	if workspaceMode == "read-only" {
		environment["GIT_OPTIONAL_LOCKS"] = "0"
	}
	for name, value := range terminalEnv(os.Environ()) {
		environment[name] = value
	}
	for name, value := range agent.Environment {
		environment[name] = value
	}
	for name, value := range commandSetup.Environment {
		environment[name] = value
	}
	for _, inherited := range []string{"LANG", "TZ"} {
		if value, found := os.LookupEnv(inherited); found {
			environment[inherited] = value
		}
	}
	for _, assignment := range opts.Env {
		name, value, found := strings.Cut(assignment, "=")
		if !found || name == "" || strings.ContainsRune(value, 0) {
			return LaunchPlan{}, fmt.Errorf("invalid --env assignment: %q", assignment)
		}
		environment[name] = value
	}
	for _, name := range opts.UnsetEnv {
		if name == "" || strings.Contains(name, "=") || strings.ContainsRune(name, 0) {
			return LaunchPlan{}, fmt.Errorf("invalid --unsetenv name: %q", name)
		}
		delete(environment, name)
	}
	// This launcher-owned status value must reflect the resolved plan. Runtime
	// security and cleanup use an internal command mode rather than trusting it.
	environment["BWRAP_AGENT_PODMAN"] = boolString(podmanBin != "")
	if podmanBin != "" {
		// The outer podman-unshare supervisor keeps the user and mount namespace
		// alive for the full launch, so neither it nor nested Podman commands
		// need a persistent pause process.
		environment["PODMAN_NO_PAUSE_PROCESS"] = "1"
		// These paths belong to the instance, regardless of host or --env settings.
		delete(environment, "CONTAINERS_CONF")
		environment["CONTAINERS_CONF_OVERRIDE"] = containersConfig
		environment["CONTAINERS_STORAGE_CONF"] = storageConfig
	}
	// Never accept a policy payload from configuration or the host environment.
	// Enabled Landlock replaces it below with a launcher-generated allowlist;
	// otherwise the sandbox runtime must not see this internal control value.
	delete(environment, internalLandlockEnvironment)
	if landlockStatus.Effective == "enabled" {
		hostWritePaths := []string{state}
		hostWritePaths = append(hostWritePaths, identity.RWBind...)
		if workspaceMode != "read-only" {
			hostWritePaths = append(hostWritePaths, project)
			if identity.GitCommon != "" {
				hostWritePaths = append(hostWritePaths, identity.GitCommon)
			}
		}
		// Bubblewrap creates these paths in the sandbox independently of whether
		// matching paths exist on the host. They are present by the time the
		// sandbox runtime applies Landlock.
		writePaths, err := prepareLandlockWritePaths(hostWritePaths, []string{"/tmp", "/var/tmp", "/run", "/dev"})
		if err != nil {
			return LaunchPlan{}, fmt.Errorf("prepare Landlock write paths: %w", err)
		}
		encoded, err := json.Marshal(writePaths)
		if err != nil {
			return LaunchPlan{}, err
		}
		environment[internalLandlockEnvironment] = string(encoded)
	}

	bwrap := []string{bwrapBin, "--unshare-ipc", "--unshare-pid", "--unshare-uts", "--unshare-cgroup-try", "--die-with-parent"}
	if podmanBin == "" {
		bwrap = append(bwrap, "--unshare-user", "--disable-userns")
	}
	if !usePTY {
		bwrap = append(bwrap, "--new-session")
	}
	bwrap = append(bwrap, "--tmpfs", "/", "--dir", "/var", "--dir", "/run", "--proc", "/proc", "--dev", "/dev", "--dir", "/dev/net", "--tmpfs", "/tmp", "--tmpfs", "/var/tmp", "--dir", "/run/bwrap-agent")
	mounts := mountBuilder{args: bwrap, made: map[string]bool{"/dev": true, "/dev/net": true, "/tmp": true, "/var": true, "/var/tmp": true, "/run": true, "/run/bwrap-agent": true}}
	var descriptorSources []string
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		mounts.mount("--dev-bind", "/dev/net/tun", "/dev/net/tun")
	}
	if _, err := os.Stat("/usr"); err == nil {
		mounts.mount("--ro-bind", "/usr", "/usr")
	}
	mounts.operation("--dir", "/etc")
	mounts.made["/etc"] = true
	if err := mountSystemEtc(&mounts, "/etc", podmanBin != ""); err != nil {
		return LaunchPlan{}, err
	}
	if err := mountSystemEtcExternalPaths(&mounts, "/"); err != nil {
		return LaunchPlan{}, err
	}
	if opts.Network == "host" {
		for _, path := range []string{"/etc/resolv.conf"} {
			if err := mountEtcPath(&mounts, "/etc", strings.TrimPrefix(path, "/etc/")); err != nil {
				return LaunchPlan{}, err
			}
			if err := mountExternalEtcLinkTarget(&mounts, path); err != nil {
				return LaunchPlan{}, err
			}
		}
	}
	for _, file := range generatedEtc {
		fd := 4 + len(descriptorSources)
		descriptorSources = append(descriptorSources, file.Source)
		mounts.parentDirs(file.Destination)
		mounts.operation("--perms", file.Permissions, "--ro-bind-data", strconv.Itoa(fd), file.Destination)
	}
	for _, legacy := range []string{"/bin", "/sbin", "/lib", "/lib64"} {
		info, err := os.Lstat(legacy)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(legacy)
			if err != nil {
				return LaunchPlan{}, err
			}
			mounts.operation("--symlink", target, legacy)
		} else {
			mounts.mount("--ro-bind", legacy, legacy)
		}
	}
	if _, err := os.Stat("/sys"); err == nil {
		option := "--ro-bind"
		if podmanBin != "" {
			// Privileged rootless containers expect to reuse the outer user
			// namespace's sysfs mount. The agent runs in a less-privileged child
			// user namespace and cannot exercise the outer namespace's sysfs
			// capabilities even though this bind remains writable.
			option = "--bind"
		}
		mounts.mount(option, "/sys", "/sys")
	}
	switch workspaceMode {
	case "write-through":
		mounts.mount("--bind", project, project)
		if identity.GitCommon != "" {
			mounts.mount("--bind", identity.GitCommon, identity.GitCommon)
		}
	case "copy-on-write":
		for _, source := range identity.ROBind {
			if pathsOverlap(source, project) || identity.GitCommon != "" && pathsOverlap(source, identity.GitCommon) {
				return LaunchPlan{}, fmt.Errorf("read-only bind %s overlaps a copy-on-write workspace hierarchy", source)
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
	mounts.mount("--bind", state, state)
	if passwdHomeAliased {
		mounts.mount("--bind", filepath.Join(state, "home"), sandboxPasswdHome)
	}
	for _, source := range identity.ROBind {
		mounts.mount("--ro-bind", source, source)
	}
	for _, source := range identity.RWBind {
		mounts.mount("--bind", source, source)
	}
	controlMounts, protectedPaths, err := prepareControlFileProtection(opts, identity)
	if err != nil {
		return LaunchPlan{}, err
	}
	for _, bind := range controlMounts {
		mounts.mount(bind.option, bind.source, bind.destination)
	}
	for _, bind := range agent.Mounts {
		mounts.mount("--ro-bind", bind.Source, bind.Destination)
	}
	if podmanBin != "" {
		mounts.mount("--ro-bind", podmanMount.Source, podmanMount.Destination)
		mounts.mount("--ro-bind", podmanMount.Source, filepath.Join(state, "home", ".config", "containers"))
	}
	self, err := os.Executable()
	if err != nil {
		return LaunchPlan{}, err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return LaunchPlan{}, err
	}
	mounts.parentDirs("/run/bwrap-agent/init")
	mounts.operation("--perms", "0555", "--ro-bind-data", "3", "/run/bwrap-agent/init")
	for _, bind := range commandSetup.Mounts {
		if bind.Executable && podmanBin != "" {
			info, err := os.Stat(bind.Source)
			if err != nil {
				return LaunchPlan{}, err
			}
			if info.Mode().IsRegular() {
				fd := 4 + len(descriptorSources)
				descriptorSources = append(descriptorSources, bind.Source)
				mounts.parentDirs(bind.Destination)
				mounts.operation("--perms", "0555", "--ro-bind-data", strconv.Itoa(fd), bind.Destination)
				continue
			}
		}
		mounts.mount("--ro-bind", bind.Source, bind.Destination)
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	bwrap = mounts.finish()
	if seccompStatus.Effective == "enabled" {
		bwrap = append(bwrap, "--seccomp", strconv.Itoa(4+len(descriptorSources)))
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
		bwrap = append(bwrap, "--unsetenv", "CONTAINERS_CONF")
	}
	initMode := internalInitMode
	if podmanBin != "" {
		initMode = internalPodmanInitMode
	}
	bwrap = append(bwrap, "--hostname", sandboxHostname, "--chdir", project, "/run/bwrap-agent/init", initMode)
	bwrap = append(bwrap, command...)
	if opts.Network == "none" {
		bwrap = append([]string{bwrap[0], "--unshare-net"}, bwrap[1:]...)
	}

	var outer []string
	if podmanBin != "" {
		outer = append(outer, podmanBin, "unshare")
	}
	if opts.Network == "private" {
		outer = append(outer, pastaBin, "--quiet", "--config-net", "--splice-only")
		if podmanBin != "" {
			outer = append(outer, "--netns-only")
		}
		tcpNamespacePorts := fmt.Sprintf("%d:%s,%d:%s", proxyGuestPort, proxyPortPlaceholder, dnsGuestPort, dnsPortPlaceholder)
		dnsNamespacePort := fmt.Sprintf("%d:%s", dnsGuestPort, dnsPortPlaceholder)
		outer = append(outer, "--tcp-ports", "none", "--udp-ports", "none", "--tcp-ns", tcpNamespacePorts, "--udp-ns", dnsNamespacePort)
		for _, port := range ports {
			option := "--tcp-ports"
			if port.Protocol == "udp" {
				option = "--udp-ports"
			}
			outer = append(outer, option, fmt.Sprintf("127.0.0.1/%d:%d", port.Host, port.Guest))
		}
		outer = append(outer, "--")
	}
	launchEnv := cloneMap(environment)
	// Podman uses USER to look up subordinate IDs before entering its user
	// namespace. Sandbox overrides must not change the invoking account.
	if podmanBin != "" {
		launchEnv["USER"], launchEnv["LOGNAME"] = hostAccount, hostAccount
	}
	// The outer podman-unshare process needs a host-visible runtime directory;
	// bubblewrap sets the shorter private value encoded in its own argv.
	launchEnv["XDG_RUNTIME_DIR"] = filepath.Join(state, "run")
	if storageConfig != "" {
		launchEnv["CONTAINERS_CONF"] = containersConfig
		// Do not load the same file twice and append duplicate pasta options.
		delete(launchEnv, "CONTAINERS_CONF_OVERRIDE")
	}
	proxyPort := 0
	if opts.Network == "private" {
		proxyPort = proxyGuestPort
	}
	launcher := []string{self, internalLaunchMode}
	if launcherTrampoline != "" {
		// Fedora's SELinux policy leaves a user-home executable launched directly
		// by pasta in pasta_t, which cannot traverse common state-home labels.
		// A system executable trampoline restores the caller domain before this
		// launcher reopens descriptor-backed inputs. Pass the launcher as the
		// shell's quoted $0 so every valid path remains an opaque argument.
		launcher = append([]string{launcherTrampoline, "-c", `exec "$0" "$@"`}, launcher...)
	}
	if seccompStatus.Effective == "enabled" {
		launcher = append(launcher, "--seccomp-profile", seccompStatus.Profile)
	}
	launcher = append(append(launcher, descriptorSources...), "--")
	return LaunchPlan{Clipboard: clipboardMode, clipboardBridge: clipboardConfig, Instance: instance, Project: project, State: state, WorkspaceMode: workspaceMode,
		Landlock: landlockStatus, Seccomp: seccompStatus, Bubblewrap: bwrapInfo.BubblewrapStatus,
		NetworkAllow: append([]string{}, opts.NetworkAllow...), ProxyGuestPort: proxyPort,
		Command: command, Outer: outer,
		Launcher: launcher,
		Bwrap:    bwrap, Ports: ports, LaunchEnv: launchEnv,
		TTY: usePTY, ConfigFiles: opts.ConfigFiles, ProtectedPaths: protectedPaths,
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
