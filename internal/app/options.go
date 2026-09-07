package app

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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
	Project       string
	Instance      string
	NoAgentConfig bool
	Network       string
	Publish       []string
	Podman        string
	WritePolicy   string
	ROBind        []string
	RWBind        []string
	Env           []string
	UnsetEnv      []string
	TTY           string
	DryRun        bool
	Command       []string
	ConfigFiles   []ConfigSource
}

// cliOptions contains the run command's options. Pointers let an omitted
// configurable scalar be distinguished from an explicit command-line override.
type cliOptions struct {
	Project         *string    `name:"project" type:"path" placeholder:"PATH" help:"Expose PATH as the project directory; write access follows --write-policy (default: current directory)."`
	Instance        *string    `name:"instance" placeholder:"NAME" help:"Use this managed instance name, overriding project configuration (default: project directory name)."`
	AgentConfig     *bool      `name:"agent-config" negatable:"" help:"Seed detected agent configuration into the instance. Default: enabled."`
	NoConfig        bool       `name:"no-config" help:"Do not load user or project configuration files."`
	NoProjectConfig bool       `name:"no-project-config" help:"Load user configuration but not the project configuration file."`
	Network         *string    `name:"network" enum:"private,host,none" placeholder:"private|host|none" help:"Network mode: private (isolated via pasta), host (shared), or none (disabled). Default: private."`
	Publish         stringList `name:"publish" placeholder:"[HOST_PORT:]GUEST_PORT[/tcp|udp]" help:"Publish a private-network port on host loopback; repeatable. HOST_PORT=0 chooses a free port. Requires --network=private."`
	Podman          *string    `name:"podman" enum:"auto,on,off" placeholder:"auto|on|off" help:"Podman mode: auto (enable if found), on (require), or off (disable). Enabled modes provide a lazy API socket. Default: auto."`
	WritePolicy     *string    `name:"write-policy" enum:"workspace,state-only" placeholder:"workspace|state-only" help:"Host write policy: workspace (project, Git metadata, state, and --rw-bind paths) or state-only (instance state only). Default: workspace."`
	ROBind          stringList `name:"ro-bind" type:"path" placeholder:"PATH" help:"Bind an additional existing host path read-only; repeatable."`
	RWBind          stringList `name:"rw-bind" type:"path" placeholder:"PATH" help:"Bind an additional existing host path read-write; repeatable. Incompatible with --write-policy=state-only."`
	Env             stringList `name:"env" placeholder:"NAME[=VALUE]" help:"Set an environment variable, or inherit NAME from the host when VALUE is omitted; repeatable."`
	UnsetEnv        stringList `name:"unsetenv" placeholder:"NAME" help:"Remove an environment variable from the sandbox; repeatable."`
	TTY             *string    `name:"tty" enum:"auto,always,never" placeholder:"auto|always|never" help:"Controlling PTY mode: auto (when stdin and stdout are terminals), always, or never. Default: auto."`
	DryRun          bool       `name:"dry-run" help:"Print the launch plan as JSON instead of starting the sandbox."`
	Command         []string   `arg:"" name:"program-and-args" passthrough:"partial" help:"Program to execute followed by its arguments."`
}

type configOptions struct {
	Create struct{} `cmd:"" help:"Create a documented .bwrap-agent.toml in the current directory."`
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
	commandConfigCreate
	commandInstanceList
	commandInstanceDelete
)

type parsedCLI struct {
	Command        commandKind
	Run            cliOptions
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
	case "config create":
		return parsedCLI{Command: commandConfigCreate}, 0, nil
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

func Main(args []string) int {
	if len(args) > 0 && args[0] == internalInitMode {
		return sandboxInit(args[1:])
	}
	if len(args) > 0 && args[0] == internalPodmanServiceMode {
		return podmanServiceExec()
	}
	parsed, code, err := parseCLI(args, os.Stdout, os.Stderr)
	if err != nil || code != 0 {
		return code
	}
	if parsed.Command == commandConfigCreate {
		if err := createProjectConfigAndReport(".", os.Stdout); err != nil {
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
	plan, err := buildPlan(opts, identity)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
		return 2
	}
	if opts.DryRun {
		if err := writePlanJSON(os.Stdout, plan); err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: failed to write plan: %v\n", err)
			return 126
		}
		return 0
	}
	for _, port := range plan.Ports {
		fmt.Fprintf(os.Stderr, "bwrap-agent: %s 127.0.0.1:%d -> sandbox 127.0.0.1:%d\n", port.Protocol, port.Host, port.Guest)
	}
	if plan.TTY {
		return runWithPTY(plan.Argv(), plan.LaunchEnv, os.Stdin, os.Stdout)
	}
	return runDirect(plan.Argv(), plan.LaunchEnv, os.Stdin, os.Stdout, os.Stderr)
}
