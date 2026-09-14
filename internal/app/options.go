package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/alecthomas/kong"
)

type stringList []string

func (s *stringList) Decode(ctx *kong.DecodeContext) error {
	var value string
	if err := ctx.Scan.PopValueInto("value", &value); err != nil {
		return err
	}
	*s = append(*s, value)
	return nil
}

// Options is the fully merged set of values used to construct a sandbox.
type Options struct {
	Clipboard              string
	Project                string
	Instance               string
	NoAgentConfig          bool
	NoMavenConfig          bool
	NoGitConfig            bool
	Network                string
	NetworkAllow           []string
	Publish                []string
	Podman                 string
	WorkspaceMode          string
	Landlock               string
	Seccomp                string
	ROBind                 []string
	RWBind                 []string
	Env                    []string
	UnsetEnv               []string
	TTY                    string
	DryRun                 bool
	AllowControlFileWrites bool
	Command                []string
	ConfigFiles            []ConfigSource
}

// cliOptions contains the run command's options. Pointers let an omitted
// configurable scalar be distinguished from an explicit command-line override.
type cliOptions struct {
	Clipboard              *string    `name:"clipboard" enum:"off,wayland" placeholder:"off|wayland" help:"Host clipboard bridge: wayland accepts terminal clipboard writes via host wl-copy. Requires a PTY. Default: off."`
	Project                *string    `name:"project" type:"path" placeholder:"PATH" help:"Expose PATH as the project directory; write behavior follows --workspace-mode (default: current directory)."`
	Instance               *string    `name:"instance" placeholder:"NAME" help:"Use this managed instance name, overriding project configuration (default: project directory name)."`
	MavenConfig            *bool      `name:"maven-config" negatable:"" help:"Inherit host Maven settings and Maven 3 security configuration read-only. Private proxy setup remains enabled. Default: enabled."`
	GitConfig              *bool      `name:"git-config" negatable:"" help:"Expose standard host user Git configuration read-only. Default: enabled."`
	AgentConfig            *bool      `name:"agent-config" negatable:"" help:"Expose detected host agent configuration read-only and seed mutable credentials. Default: enabled."`
	NoConfig               bool       `name:"no-config" help:"Do not load user or project configuration files."`
	NoProjectConfig        bool       `name:"no-project-config" help:"Load user configuration but not the project configuration file."`
	Network                *string    `name:"network" enum:"private,host,none" placeholder:"private|host|none" help:"Network mode: private (HTTP/HTTPS allowlist enforced), host (shared and unrestricted), or none (disabled). Default: private."`
	NetworkAllow           stringList `name:"network-allow" placeholder:"ORIGIN" help:"Allow an HTTP/HTTPS origin in private mode; repeatable (for example https://registry.example.com or https://*.example.com)."`
	Publish                stringList `name:"publish" placeholder:"[HOST_PORT:]GUEST_PORT[/tcp|udp]" help:"Publish a private-network port on host loopback; repeatable. HOST_PORT=0 chooses a free port. Requires --network=private."`
	Podman                 *string    `name:"podman" enum:"auto,on,off" placeholder:"auto|on|off" help:"Podman mode: auto (enable if compatible and found), on (require), or off (disable). Enabled modes provide a lazy API socket. Read-only workspaces and required Landlock disable auto. Default: auto."`
	WorkspaceMode          *string    `name:"workspace-mode" enum:"write-through,copy-on-write,read-only" placeholder:"write-through|copy-on-write|read-only" help:"Workspace behavior: write-through (persist changes), copy-on-write (discard changes), or read-only. Default: write-through."`
	Landlock               *string    `name:"landlock" enum:"auto,required,off" placeholder:"auto|required|off" help:"Landlock filesystem enforcement: auto (when compatible), required (fail closed), or off. Default: auto."`
	Seccomp                *string    `name:"seccomp" enum:"auto,required,off" placeholder:"auto|required|off" help:"Seccomp syscall filtering: auto (enable when supported), required (fail closed), or off. Default: auto."`
	ROBind                 stringList `name:"ro-bind" type:"path" placeholder:"PATH" help:"Bind an additional existing host path read-only; repeatable."`
	RWBind                 stringList `name:"rw-bind" type:"path" placeholder:"PATH" help:"Bind an additional existing host path read-write; repeatable."`
	Env                    stringList `name:"env" placeholder:"NAME[=VALUE]" help:"Set an environment variable, or inherit NAME from the host when VALUE is omitted; repeatable."`
	UnsetEnv               stringList `name:"unsetenv" placeholder:"NAME" help:"Remove an environment variable from the sandbox; repeatable."`
	TTY                    *string    `name:"tty" enum:"auto,always,never" placeholder:"auto|always|never" help:"Controlling PTY mode: auto (when stdin and stdout are terminals), always, or never. Default: auto."`
	DryRun                 bool       `name:"dry-run" help:"Print the launch plan as JSON instead of starting the sandbox."`
	AllowControlFileWrites bool       `name:"allow-control-file-writes" help:"Disable built-in read-only protection for Git and agent control paths for this run."`
	Command                []string   `arg:"" name:"program-and-args" passthrough:"partial" help:"Program to execute followed by its arguments."`
}

type configTrustOptions struct {
	Project string `name:"project" type:"path" default:"." help:"Project directory whose configuration approval to manage."`
}

type configOptions struct {
	Trust   configTrustOptions `cmd:"" help:"Approve the exact contents of project configuration after reviewing it."`
	Untrust configTrustOptions `cmd:"" help:"Remove approval for project configuration."`
	Create  struct {
		Project struct{} `cmd:"" help:"Create a documented .bwrap-agent.toml in the current directory."`
		User    struct{} `cmd:"" help:"Create documented configuration for the current user across all projects."`
	} `cmd:"" help:"Create a documented project or user configuration file."`
}

type instanceListOptions struct {
	JSON bool `name:"json" help:"Write machine-readable JSON."`
}

type instanceDeleteOptions struct {
	Name string `arg:"" name:"name" help:"Managed instance name."`
	Yes  bool   `name:"yes" short:"y" help:"Delete without interactive confirmation."`
}

type instanceOptions struct {
	List   instanceListOptions   `cmd:"" help:"List managed instances."`
	Delete instanceDeleteOptions `cmd:"" help:"Delete a stopped managed instance."`
}

type rootOptions struct {
	Run      cliOptions      `cmd:"" help:"Run a program in a sandbox."`
	Config   configOptions   `cmd:"" help:"Manage configuration."`
	Instance instanceOptions `cmd:"" help:"Manage persistent instances."`
}

type commandKind uint8

const (
	commandRun commandKind = iota + 1
	commandConfigCreateProject
	commandConfigCreateUser
	commandConfigTrust
	commandConfigUntrust
	commandInstanceList
	commandInstanceDelete
)

type parsedCLI struct {
	Command        commandKind
	Run            cliOptions
	ConfigTrust    configTrustOptions
	InstanceList   instanceListOptions
	InstanceDelete instanceDeleteOptions
}

type cliParseError struct{ err error }

func (e cliParseError) Error() string { return e.err.Error() }

var errCLIHandled = errors.New("CLI request handled")

func parseCLI(args []string, stdout, stderr io.Writer) (parsed parsedCLI, code int, err error) {
	if len(args) == 0 {
		args = []string{"--help"}
	}
	var root rootOptions
	parser, err := kong.New(
		&root,
		kong.Name("bwrap-agent"),
		kong.Description("Run programs in constrained sandboxes and manage persistent instances and configuration."),
		kong.Writers(stdout, stderr),
		kong.UsageOnError(),
		kong.Exit(func(exitCode int) { panic(cliParseError{err: fmt.Errorf("exit %d", exitCode)}) }),
	)
	if err != nil {
		return parsed, 2, err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if parseExit, ok := recovered.(cliParseError); ok {
				code = parseExitCode(parseExit.err)
				err = errCLIHandled
				return
			}
			panic(recovered)
		}
	}()
	context, err := parser.Parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "bwrap-agent: %v\n", err)
		return parsed, 2, err
	}
	switch context.Command() {
	case "run <program-and-args>":
		if len(root.Run.Command) > 0 && root.Run.Command[0] == "--" {
			root.Run.Command = root.Run.Command[1:]
		}
		if len(root.Run.Command) == 0 {
			err = errors.New("PROGRAM is required (for example: bwrap-agent run opencode)")
			fmt.Fprintf(stderr, "bwrap-agent: %v\n", err)
			return parsed, 2, err
		}
		return parsedCLI{Command: commandRun, Run: root.Run}, 0, nil
	case "config create project":
		return parsedCLI{Command: commandConfigCreateProject}, 0, nil
	case "config create user":
		return parsedCLI{Command: commandConfigCreateUser}, 0, nil
	case "config trust":
		return parsedCLI{Command: commandConfigTrust, ConfigTrust: root.Config.Trust}, 0, nil
	case "config untrust":
		return parsedCLI{Command: commandConfigUntrust, ConfigTrust: root.Config.Untrust}, 0, nil
	case "instance list":
		return parsedCLI{Command: commandInstanceList, InstanceList: root.Instance.List}, 0, nil
	case "instance delete <name>":
		return parsedCLI{Command: commandInstanceDelete, InstanceDelete: root.Instance.Delete}, 0, nil
	default:
		err = fmt.Errorf("unsupported command %q", context.Command())
		fmt.Fprintf(stderr, "bwrap-agent: %v\n", err)
		return parsed, 2, err
	}
}

func mergeCLIOptions(cli cliOptions, stderr io.Writer) (Options, int, error) {
	projectInput := "."
	if cli.Project != nil {
		projectInput = *cli.Project
	}
	project, err := resolveProjectDirectory(projectInput)
	if err != nil {
		fmt.Fprintf(stderr, "bwrap-agent: %v\n", err)
		return Options{}, 2, err
	}
	layers, sources, err := loadConfiguration(project, cli.NoConfig, cli.NoProjectConfig)
	if err != nil {
		fmt.Fprintf(stderr, "bwrap-agent: %v\n", err)
		return Options{}, 2, err
	}
	opts, err := mergeOptions(cli, project, layers, sources, os.Environ())
	if err != nil {
		fmt.Fprintf(stderr, "bwrap-agent: %v\n", err)
		return Options{}, 2, err
	}
	return opts, 0, nil
}

func parseOptions(args []string, stdout, stderr io.Writer) (Options, int, error) {
	parsed, code, err := parseCLI(args, stdout, stderr)
	if err != nil || code != 0 {
		return Options{}, code, err
	}
	if parsed.Command != commandRun {
		err := errors.New("expected the run command")
		fmt.Fprintf(stderr, "bwrap-agent: %v\n", err)
		return Options{}, 2, err
	}
	return mergeCLIOptions(parsed.Run, stderr)
}

func parseExitCode(err error) int {
	if err == nil {
		return 0
	}
	parts := strings.Fields(err.Error())
	if len(parts) == 2 && parts[0] == "exit" && parts[1] == "0" {
		return 0
	}
	return 2
}

func Main(args []string) (status int) {
	if len(args) > 0 && args[0] == internalInitMode {
		return sandboxInit(args[1:], false)
	}
	if len(args) > 0 && args[0] == internalPodmanInitMode {
		return sandboxInit(args[1:], true)
	}
	if len(args) > 0 && args[0] == internalRuntimeMode {
		return sandboxRuntime(args[1:], true)
	}
	if len(args) > 0 && args[0] == internalPodmanServiceMode {
		return podmanServiceExec()
	}
	if len(args) > 0 && args[0] == internalLaunchMode {
		return launchBubblewrap(args[1:])
	}
	parsed, code, err := parseCLI(args, os.Stdout, os.Stderr)
	if err != nil || code != 0 {
		return code
	}
	if parsed.Command == commandConfigTrust || parsed.Command == commandConfigUntrust {
		if err := setProjectConfigTrust(parsed.ConfigTrust.Project, parsed.Command == commandConfigTrust, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
			return 2
		}
		return 0
	}
	if parsed.Command == commandConfigCreateProject || parsed.Command == commandConfigCreateUser {
		var err error
		if parsed.Command == commandConfigCreateProject {
			err = createProjectConfigAndReport(".", os.Stdout)
		} else {
			err = createUserConfigAndReport(os.Stdout)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
			return 2
		}
		return 0
	}
	if parsed.Command == commandInstanceList {
		if err := listInstances(parsed.InstanceList.JSON, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
			return 2
		}
		return 0
	}
	if parsed.Command == commandInstanceDelete {
		if err := deleteInstance(parsed.InstanceDelete.Name, parsed.InstanceDelete.Yes,
			isTerminal(os.Stdin.Fd()), os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
			return 2
		}
		return 0
	}
	opts, code, err := mergeCLIOptions(parsed.Run, os.Stderr)
	if err != nil || code != 0 {
		return code
	}
	identity, lock, err := resolveAndLockInstance(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
		return 2
	}
	defer lock.Close()
	if err := markInstanceUsed(identity); err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: update instance metadata: %v\n", err)
		return 2
	}
	var launchSignals chan os.Signal
	if !opts.DryRun {
		launchSignals = make(chan os.Signal, 8)
		signal.Notify(launchSignals, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
		defer signal.Stop(launchSignals)
	}
	plan, err := buildPlan(opts, identity)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
		return 2
	}
	if plan.Bubblewrap.Legacy {
		fmt.Fprintf(os.Stderr, "bwrap-agent: warning: Bubblewrap %s is a temporary compatibility tier; upgrade to 0.12 or newer for the supported security boundary\n", plan.Bubblewrap.Version)
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(os.Stderr, "bwrap-agent: warning: %s\n", warning)
	}
	if opts.AllowControlFileWrites && plan.WorkspaceMode == "write-through" {
		fmt.Fprintln(os.Stderr, "bwrap-agent: warning: built-in control-file write protection is disabled for this run")
	}
	if opts.DryRun {
		if err := writePlanJSON(os.Stdout, plan); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: failed to write plan: %v\n", err)
			return 126
		}
		return 0
	}
	if plan.LaunchEnv["BWRAP_AGENT_PODMAN"] == "1" {
		root, launchEnv, err := createPodmanBootstrap(plan.LaunchEnv, identity)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
			return 126
		}
		defer func() {
			if err := removePodmanBootstrap(root); err != nil {
				fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
				if status == 0 {
					status = 126
				}
			}
		}()
		plan.LaunchEnv = launchEnv
	}
	argv := plan.Argv()
	var proxy *networkProxy
	if plan.ProxyGuestPort != 0 {
		policy, policyErr := parseNetworkPolicy(plan.NetworkAllow)
		if policyErr != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: invalid network allowlist: %v\n", policyErr)
			return 2
		}
		proxy, err = startNetworkProxy(policy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: start network policy proxy: %v\n", err)
			return 126
		}
		defer proxy.Close()
		argv, err = plan.runtimeArgv(proxy.port(), proxy.dnsPort())
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: prepare private network: %v\n", err)
			return 126
		}
	}

	select {
	case received := <-launchSignals:
		if sig, ok := received.(syscall.Signal); ok {
			return 128 + int(sig)
		}
		return 126
	default:
	}
	for _, port := range plan.Ports {
		fmt.Fprintf(os.Stderr, "bwrap-agent: %s 127.0.0.1:%d -> sandbox 127.0.0.1:%d\n", port.Protocol, port.Host, port.Guest)
	}
	if plan.TTY {
		signal.Notify(launchSignals, syscall.SIGWINCH)
		status = runWithPTYClipboard(argv, plan.LaunchEnv, os.Stdin, os.Stdout, launchSignals, plan.clipboardBridge)
	} else {
		status = runDirectSignals(argv, plan.LaunchEnv, os.Stdin, os.Stdout, os.Stderr, launchSignals)
	}
	return status
}
