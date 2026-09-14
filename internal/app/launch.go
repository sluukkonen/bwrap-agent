//go:build linux

package app

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// launchSession owns host resources until the outer sandbox process has exited.
// It is used by one launch goroutine; Close also handles partial preparation.
type launchSession struct {
	identity  instanceIdentity
	lock      *instanceLock
	signals   chan os.Signal
	bootstrap string
	proxy     *networkProxy
}

func (session *launchSession) Close() error {
	if session.proxy != nil {
		_ = session.proxy.Close()
		session.proxy = nil
	}
	var err error
	if session.bootstrap != "" {
		err = removePodmanBootstrap(session.bootstrap)
		session.bootstrap = ""
	}
	if session.signals != nil {
		signal.Stop(session.signals)
		session.signals = nil
	}
	if session.lock != nil {
		_ = session.lock.Close()
		session.lock = nil
	}
	return err
}

func (session *launchSession) finish(status int) int {
	if err := session.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
		if status == 0 {
			return 126
		}
	}
	return status
}

func runLaunch(opts Options) (status int) {
	identity, lock, err := resolveAndLockInstance(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
		return 2
	}
	session := &launchSession{identity: identity, lock: lock}
	defer func() { status = session.finish(status) }()
	if err := markInstanceUsed(identity); err != nil {
		fmt.Fprintf(os.Stderr, "bwrap-agent: update instance metadata: %v\n", err)
		return 2
	}
	if !opts.DryRun {
		session.signals = make(chan os.Signal, 8)
		signal.Notify(session.signals, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
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
	return session.run(plan)
}

func (session *launchSession) run(plan LaunchPlan) int {
	environment := plan.LaunchEnv
	if plan.LaunchEnv["BWRAP_AGENT_PODMAN"] == "1" {
		root, launchEnv, err := createPodmanBootstrap(plan.LaunchEnv, session.identity)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: %v\n", err)
			return 126
		}
		session.bootstrap = root
		environment = launchEnv
	}
	argv := plan.Argv()
	var err error
	if plan.ProxyGuestPort != 0 {
		policy, policyErr := parseNetworkPolicy(plan.NetworkAllow)
		if policyErr != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: invalid network allowlist: %v\n", policyErr)
			return 2
		}
		session.proxy, err = startNetworkProxy(policy)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: start network policy proxy: %v\n", err)
			return 126
		}
		argv, err = plan.runtimeArgv(session.proxy.port(), session.proxy.dnsPort())
		if err != nil {
			fmt.Fprintf(os.Stderr, "bwrap-agent: prepare private network: %v\n", err)
			return 126
		}
	}

	select {
	case received := <-session.signals:
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
		signal.Notify(session.signals, syscall.SIGWINCH)
		return runWithPTYClipboard(argv, environment, os.Stdin, os.Stdout, session.signals, plan.clipboardBridge)
	} else {
		return runDirectSignals(argv, environment, os.Stdin, os.Stdout, os.Stderr, session.signals)
	}
}
