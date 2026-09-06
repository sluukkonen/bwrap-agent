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
	Project  string
	Instance string
	State    string
	LockPath string
	Root     string
	Managed  bool
}

func resolveInstance(opts Options) (instanceIdentity, error) {
	project, err := resolveProjectDirectory(opts.Project)
	if err != nil {
		return instanceIdentity{}, err
	}
	if opts.StateDir != "" {
		instance := opts.Instance
		if instance == "" {
			instance = defaultName(project)
		}
		if instance, err = safeName(instance); err != nil {
			return instanceIdentity{}, err
		}
		state, err := resolveState(opts.StateDir)
		if err != nil {
			return instanceIdentity{}, err
		}
		return instanceIdentity{Project: project, Instance: instance, State: state, LockPath: state}, nil
	}
	return resolveManagedInstance(project, opts.Instance)
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

func resolveWritePolicy(policy string) (string, error) {
	if policy == "" {
		policy = "workspace"
	}
	if policy != "workspace" && policy != "state-only" {
		return "", fmt.Errorf("invalid write policy %q: expected workspace or state-only", policy)
	}
	return policy, nil
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

func writeStorageConfig(state string) (string, error) {
	if _, err := ensureStateDirectory(state, "config/containers", 0o700); err != nil {
		return "", err
	}
	graphRoot, err := ensureStateDirectory(state, "podman/storage", 0o700)
	if err != nil {
		return "", err
	}
	runRoot, err := ensureStateDirectory(state, "run/containers", 0o700)
	if err != nil {
		return "", err
	}
	graphJSON, _ := json.Marshal(graphRoot)
	runJSON, _ := json.Marshal(runRoot)
	content := fmt.Sprintf("[storage]\ndriver = \"overlay\"\ngraphroot = %s\nrunroot = %s\n\n[storage.options.overlay]\nignore_chown_errors = \"false\"\n", graphJSON, runJSON)
	return writeStateFile(state, "config/containers/storage.conf", []byte(content), 0o600)
}

func writeContainersConfig(state string) (string, error) {
	content := []byte("[engine]\ncgroup_manager = \"cgroupfs\"\nevents_logger = \"file\"\n")
	return writeStateFile(state, "config/containers/containers.conf", content, 0o600)
}

func writePrivateResolvConf(state string) (string, error) {
	content := []byte("# Generated by bwrap-agent for the private pasta namespace\nnameserver 169.254.1.1\n")
	return writeStateFile(state, "config/resolv.conf", content, 0o600)
}

func externalGitCommonDir(project string) string {
	command := exec.Command("git", "-C", project, "rev-parse", "--path-format=absolute", "--git-common-dir")
	command.Stderr = nil
	output, err := command.Output()
	if err != nil {
		return ""
	}
	common, err := resolveExisting(strings.TrimSpace(string(output)))
	if err != nil {
		return ""
	}
	relative, err := filepath.Rel(project, common)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ""
	}
	return common
}

func prepareOpenCode(state string) error {
	if err := secureMkdir(state, 0o700); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configBase := envValue(os.Environ(), "XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	dataBase := envValue(os.Environ(), "XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	destinationConfig := filepath.Join(state, "config", "opencode")
	destinationData := filepath.Join(state, "data", "opencode")
	if _, err := ensureStateDirectory(state, "config", 0o700); err != nil {
		return err
	}
	sourceInfo, sourceErr := os.Stat(filepath.Join(configBase, "opencode"))
	if sourceErr == nil && sourceInfo.IsDir() && !pathExists(destinationConfig) {
		if err := copyTree(filepath.Join(configBase, "opencode"), destinationConfig); err != nil {
			return err
		}
	} else if _, err := ensureStateDirectory(state, "config/opencode", 0o700); err != nil {
		return err
	}
	if _, err := ensureStateDirectory(state, "data/opencode", 0o700); err != nil {
		return err
	}
	sourceAuth := filepath.Join(dataBase, "opencode", "auth.json")
	destinationAuth := filepath.Join(destinationData, "auth.json")
	if info, err := os.Stat(sourceAuth); err == nil && info.Mode().IsRegular() && !pathExists(destinationAuth) {
		content, err := os.ReadFile(sourceAuth)
		if err != nil {
			return err
		}
		_, err = writeStateFile(state, "data/opencode/auth.json", content, 0o600)
		return err
	}
	return nil
}

func prepareAgent(program, state string) error {
	base := filepath.Base(program)
	if base == "opencode" || base == "opencode.exe" {
		return prepareOpenCode(state)
	}
	return nil
}

func resolveCommand(requested []string) ([]string, string, error) {
	found, err := exec.LookPath(requested[0])
	if err != nil {
		return nil, "", fmt.Errorf("command not found: %s", requested[0])
	}
	absolute, err := filepath.Abs(found)
	if err != nil {
		return nil, "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, "", err
	}
	for _, prefix := range []string{"/usr/", "/bin/", "/sbin/", "/lib/", "/lib64/"} {
		if strings.HasPrefix(resolved, prefix) {
			return append([]string(nil), requested...), "", nil
		}
	}
	command := append([]string{"/run/bwrap-agent/command"}, requested[1:]...)
	return command, resolved, nil
}

type mountBuilder struct {
	args []string
	made map[string]bool
}

func (m *mountBuilder) parentDirs(path string) {
	var parents []string
	for parent := filepath.Dir(path); parent != "/" && parent != "."; parent = filepath.Dir(parent) {
		parents = append(parents, parent)
	}
	for index := len(parents) - 1; index >= 0; index-- {
		if !m.made[parents[index]] {
			m.args = append(m.args, "--dir", parents[index])
			m.made[parents[index]] = true
		}
	}
}

func (m *mountBuilder) mount(option, source, destination string) {
	m.parentDirs(destination)
	m.args = append(m.args, option, source, destination)
}

func BuildPlan(opts Options) (LaunchPlan, error) {
	identity, err := resolveInstance(opts)
	if err != nil {
		return LaunchPlan{}, err
	}
	return buildPlan(opts, identity)
}

func buildPlan(opts Options, identity instanceIdentity) (LaunchPlan, error) {
	project, instance, state := identity.Project, identity.Instance, identity.State
	writePolicy, err := resolveWritePolicy(opts.WritePolicy)
	if err != nil {
		return LaunchPlan{}, err
	}
	if writePolicy == "state-only" && len(opts.RWBind) > 0 {
		return LaunchPlan{}, errors.New("--rw-bind is incompatible with --write-policy=state-only")
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
	podmanBin, err := resolvePodman(opts.Podman)
	if err != nil {
		return LaunchPlan{}, err
	}
	var storageConfig string
	if podmanBin != "" {
		storageConfig, err = writeStorageConfig(state)
		if err != nil {
			return LaunchPlan{}, err
		}
		if _, err := writeContainersConfig(state); err != nil {
			return LaunchPlan{}, err
		}
	}
	var pastaBin string
	if opts.Network == "private" {
		pastaBin, err = requireProgram("pasta")
		if err != nil {
			return LaunchPlan{}, err
		}
	}
	if !opts.NoAgentConfig {
		if err := prepareAgent(opts.Command[0], state); err != nil {
			return LaunchPlan{}, err
		}
	}
	command, externalCommand, err := resolveCommand(opts.Command)
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

	environment := map[string]string{
		"HOME": filepath.Join(state, "home"), "USER": envValue(os.Environ(), "USER", "agent"),
		"LOGNAME":         envValue(os.Environ(), "LOGNAME", envValue(os.Environ(), "USER", "agent")),
		"PATH":            "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"XDG_CONFIG_HOME": filepath.Join(state, "config"), "XDG_CACHE_HOME": filepath.Join(state, "home", ".cache"),
		"XDG_DATA_HOME": filepath.Join(state, "data"), "XDG_STATE_HOME": filepath.Join(state, "home", ".local", "state"),
		"XDG_RUNTIME_DIR": filepath.Join(state, "run"), "TMPDIR": filepath.Join(state, "tmp"),
		"BWRAP_AGENT_INSTANCE": instance, "BWRAP_AGENT_PODMAN": boolString(podmanBin != ""),
	}
	if writePolicy == "state-only" {
		environment["GIT_OPTIONAL_LOCKS"] = "0"
	}
	for name, value := range terminalEnv(os.Environ()) {
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

	bwrap := []string{bwrapBin, "--unshare-ipc", "--unshare-pid", "--unshare-uts", "--unshare-cgroup-try", "--die-with-parent"}
	if !usePTY {
		bwrap = append(bwrap, "--new-session")
	}
	bwrap = append(bwrap, "--tmpfs", "/", "--dir", "/var", "--dir", "/run", "--proc", "/proc", "--dev", "/dev", "--dir", "/dev/net", "--tmpfs", "/tmp", "--tmpfs", "/var/tmp", "--dir", "/run/bwrap-agent")
	mounts := mountBuilder{args: bwrap, made: map[string]bool{"/dev": true, "/dev/net": true, "/tmp": true, "/var": true, "/var/tmp": true, "/run": true, "/run/bwrap-agent": true}}
	if _, err := os.Stat("/dev/net/tun"); err == nil {
		mounts.mount("--dev-bind", "/dev/net/tun", "/dev/net/tun")
	}
	if _, err := os.Stat("/usr"); err == nil {
		mounts.mount("--ro-bind", "/usr", "/usr")
	}
	if opts.Network == "private" {
		mounts.args = append(mounts.args, "--dir", "/etc")
		mounts.made["/etc"] = true
		entries, err := os.ReadDir("/etc")
		if err != nil {
			return LaunchPlan{}, err
		}
		for _, entry := range entries {
			if entry.Name() == "resolv.conf" {
				continue
			}
			source := filepath.Join("/etc", entry.Name())
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(source)
				if err != nil {
					return LaunchPlan{}, err
				}
				mounts.args = append(mounts.args, "--symlink", target, source)
			} else {
				mounts.mount("--ro-bind", source, source)
			}
		}
		resolv, err := writePrivateResolvConf(state)
		if err != nil {
			return LaunchPlan{}, err
		}
		mounts.mount("--ro-bind", resolv, "/etc/resolv.conf")
	} else if _, err := os.Stat("/etc"); err == nil {
		mounts.mount("--ro-bind", "/etc", "/etc")
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
			mounts.args = append(mounts.args, "--symlink", target, legacy)
		} else {
			mounts.mount("--ro-bind", legacy, legacy)
		}
	}
	for _, optional := range []string{"/sys", "/opt", "/nix/store"} {
		if _, err := os.Stat(optional); err == nil {
			mounts.mount("--ro-bind", optional, optional)
		}
	}
	workspaceMount := "--bind"
	if writePolicy == "state-only" {
		workspaceMount = "--ro-bind"
	}
	mounts.mount(workspaceMount, project, project)
	if common := externalGitCommonDir(project); common != "" {
		mounts.mount(workspaceMount, common, common)
	}
	// State is the writable exception, even if a custom state path is nested
	// beneath the project or its external Git metadata.
	mounts.mount("--bind", state, state)
	for _, source := range opts.ROBind {
		resolved, err := resolveExisting(source)
		if err != nil {
			return LaunchPlan{}, err
		}
		mounts.mount("--ro-bind", resolved, resolved)
	}
	for _, source := range opts.RWBind {
		resolved, err := resolveExisting(source)
		if err != nil {
			return LaunchPlan{}, err
		}
		mounts.mount("--bind", resolved, resolved)
	}
	self, err := os.Executable()
	if err != nil {
		return LaunchPlan{}, err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return LaunchPlan{}, err
	}
	mounts.mount("--ro-bind", self, "/run/bwrap-agent/init")
	if externalCommand != "" {
		mounts.mount("--ro-bind", externalCommand, "/run/bwrap-agent/command")
	}
	names := make([]string, 0, len(environment))
	for name := range environment {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mounts.args = append(mounts.args, "--setenv", name, environment[name])
	}
	mounts.args = append(mounts.args, "--hostname", "agent-"+truncate(instance, 48), "--chdir", project, "/run/bwrap-agent/init", internalInitMode)
	mounts.args = append(mounts.args, command...)
	bwrap = mounts.args
	if opts.Network == "none" {
		bwrap = append([]string{bwrap[0], "--unshare-net"}, bwrap[1:]...)
	}

	var outer []string
	if podmanBin != "" {
		outer = append(outer, podmanBin, "unshare")
	}
	if opts.Network == "private" {
		outer = append(outer, pastaBin, "--quiet", "--config-net")
		if podmanBin != "" {
			outer = append(outer, "--netns-only")
		}
		outer = append(outer, "--dns-forward", "169.254.1.1", "--host-lo-to-ns-lo", "--tcp-ports", "none", "--udp-ports", "none", "--tcp-ns", "none", "--udp-ns", "none")
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
	if storageConfig != "" {
		launchEnv["CONTAINERS_STORAGE_CONF"] = storageConfig
	}
	return LaunchPlan{Instance: instance, Project: project, State: state, WritePolicy: writePolicy, Command: command, Outer: outer, Bwrap: bwrap, Ports: ports, LaunchEnv: launchEnv, TTY: usePTY, ConfigFiles: opts.ConfigFiles}, nil
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
