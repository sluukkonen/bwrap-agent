//go:build linux && (amd64 || arm64)

package app

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

type testSeccompData struct {
	number uint32
	arch   uint32
	arg0   uint32
}

func evaluateSeccompFilter(t *testing.T, program []unix.SockFilter, data testSeccompData) uint32 {
	t.Helper()
	accumulator := uint32(0)
	for pc := 0; pc < len(program); {
		instruction := program[pc]
		switch instruction.Code {
		case unix.BPF_LD | unix.BPF_W | unix.BPF_ABS:
			switch instruction.K {
			case seccompDataNumberOffset:
				accumulator = data.number
			case seccompDataArchOffset:
				accumulator = data.arch
			case seccompDataArgsOffset:
				accumulator = data.arg0
			default:
				t.Fatalf("unexpected seccomp data offset %d", instruction.K)
			}
			pc++
		case unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K:
			if accumulator == instruction.K {
				pc += 1 + int(instruction.Jt)
			} else {
				pc += 1 + int(instruction.Jf)
			}
		case unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K:
			if accumulator&instruction.K != 0 {
				pc += 1 + int(instruction.Jt)
			} else {
				pc += 1 + int(instruction.Jf)
			}
		case unix.BPF_RET | unix.BPF_K:
			return instruction.K
		default:
			t.Fatalf("unexpected BPF instruction %#v", instruction)
		}
	}
	t.Fatal("seccomp filter reached the end without returning")
	return 0
}

func seccompSyscallNumber(t *testing.T, name string) uint32 {
	t.Helper()
	number, found := nativeSeccompArchitecture.syscalls[name]
	if !found {
		t.Fatalf("missing syscall mapping for %s", name)
	}
	return number
}

func TestSeccompProfilesEnforceExpectedRules(t *testing.T) {
	development, err := compileSeccompFilter(seccompProfileDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	podman, err := compileSeccompFilter(seccompProfilePodman)
	if err != nil {
		t.Fatal(err)
	}
	evaluate := func(program []unix.SockFilter, name string, arg0 uint32) uint32 {
		return evaluateSeccompFilter(t, program, testSeccompData{
			number: seccompSyscallNumber(t, name), arch: nativeSeccompArchitecture.audit, arg0: arg0,
		})
	}
	for _, name := range []string{"acct", "init_module", "kexec_load", "swapon", "syslog"} {
		if got := evaluate(development, name, 0); got != seccompErrno(unix.EPERM) {
			t.Errorf("development %s action = %#x", name, got)
		}
		if got := evaluate(podman, name, 0); got != seccompErrno(unix.EPERM) {
			t.Errorf("Podman %s action = %#x", name, got)
		}
	}
	for _, name := range nativeSeccompArchitecture.commonSyscalls {
		if got := evaluate(development, name, 0); got != seccompErrno(unix.EPERM) {
			t.Errorf("development architecture-specific %s action = %#x", name, got)
		}
		if got := evaluate(podman, name, 0); got != seccompErrno(unix.EPERM) {
			t.Errorf("Podman architecture-specific %s action = %#x", name, got)
		}
	}
	for _, name := range []string{"bpf", "clock_settime", "keyctl", "quotactl", "open_by_handle_at", "mount", "reboot"} {
		if got := evaluate(development, name, 0); got != seccompErrno(unix.EPERM) {
			t.Errorf("development %s action = %#x", name, got)
		}
		if got := evaluate(podman, name, 0); got != unix.SECCOMP_RET_ALLOW {
			t.Errorf("Podman %s action = %#x", name, got)
		}
	}
	for _, name := range []string{"clone3", "open_tree", "mount_setattr"} {
		if got := evaluate(development, name, 0); got != seccompErrno(unix.ENOSYS) {
			t.Errorf("development %s action = %#x", name, got)
		}
	}
	for _, flag := range []uint32{
		unix.CLONE_NEWTIME, unix.CLONE_NEWNS, unix.CLONE_NEWCGROUP, unix.CLONE_NEWUTS,
		unix.CLONE_NEWIPC, unix.CLONE_NEWUSER, unix.CLONE_NEWPID, unix.CLONE_NEWNET,
	} {
		if got := evaluate(development, "clone", flag); got != seccompErrno(unix.EPERM) {
			t.Errorf("namespace clone flag %#x action = %#x", flag, got)
		}
	}
	if got := evaluate(development, "clone", uint32(unix.SIGCHLD)); got != unix.SECCOMP_RET_ALLOW {
		t.Errorf("ordinary clone action = %#x", got)
	}
	if got := evaluate(podman, "clone3", 0); got != unix.SECCOMP_RET_ALLOW {
		t.Errorf("Podman clone3 action = %#x", got)
	}
}

func TestSeccompFilterRejectsAlternateABIs(t *testing.T) {
	program, err := compileSeccompFilter(seccompProfileDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	if got := evaluateSeccompFilter(t, program, testSeccompData{number: seccompSyscallNumber(t, "clone"), arch: 0}); got != unix.SECCOMP_RET_KILL_PROCESS {
		t.Fatalf("foreign architecture action = %#x", got)
	}
	if nativeSeccompArchitecture.compatMask != 0 {
		if got := evaluateSeccompFilter(t, program, testSeccompData{
			number: seccompSyscallNumber(t, "clone") | nativeSeccompArchitecture.compatMask,
			arch:   nativeSeccompArchitecture.audit,
		}); got != unix.SECCOMP_RET_KILL_PROCESS {
			t.Fatalf("compat ABI action = %#x", got)
		}
	}
}

func TestSeccompFilterSerialization(t *testing.T) {
	program, err := compileSeccompFilter(seccompProfileDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	content, err := marshalSeccompFilter(program)
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != len(program)*8 {
		t.Fatalf("serialized filter length = %d, want %d", len(content), len(program)*8)
	}
	filter, err := openSeccompFilter(seccompProfileDevelopment)
	if err != nil {
		t.Fatal(err)
	}
	defer filter.Close()
	read, err := io.ReadAll(filter)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, content) {
		t.Fatal("filter pipe content differs from serialized program")
	}
	if _, err := compileSeccompFilter("unknown"); err == nil {
		t.Fatal("unknown seccomp profile was accepted")
	}
}

func TestDetermineSeccompStatus(t *testing.T) {
	if !nativeSeccompArchitecture.supported {
		t.Skip("native architecture intentionally unsupported")
	}
	tests := []struct {
		mode      string
		podman    bool
		supported bool
		queryErr  error
		effective string
		profile   string
		warning   bool
		wantErr   bool
	}{
		{mode: "off", effective: "off"},
		{mode: "auto", supported: true, effective: "enabled", profile: seccompProfileDevelopment},
		{mode: "auto", podman: true, supported: true, effective: "enabled", profile: seccompProfilePodman},
		{mode: "auto", effective: "unsupported", warning: true},
		{mode: "auto", queryErr: unix.EPERM, effective: "unsupported", warning: true},
		{mode: "required", wantErr: true},
	}
	for _, test := range tests {
		called := false
		status, warning, err := determineSeccompStatus(test.mode, test.podman, func() (bool, error) {
			called = true
			return test.supported, test.queryErr
		})
		if test.mode == "off" && called {
			t.Error("off mode queried seccomp support")
		}
		if (err != nil) != test.wantErr {
			t.Errorf("mode %s error = %v", test.mode, err)
			continue
		}
		if err == nil && (status.Effective != test.effective || status.Profile != test.profile || (warning != "") != test.warning) {
			t.Errorf("mode %s status = %#v warning=%q", test.mode, status, warning)
		}
	}
}

func TestResolveSeccompMode(t *testing.T) {
	for input, want := range map[string]string{"": "auto", "auto": "auto", "required": "required", "off": "off"} {
		got, err := resolveSeccompMode(input)
		if err != nil || got != want {
			t.Errorf("resolveSeccompMode(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := resolveSeccompMode("invalid"); err == nil {
		t.Fatal("invalid seccomp mode was accepted")
	}
}

func TestSeccompKernelEnforcement(t *testing.T) {
	if os.Getenv("BWRAP_AGENT_SECCOMP_TEST_HELPER") == "1" {
		runtime.LockOSThread()
		program, err := compileSeccompFilter(seccompProfileDevelopment)
		if err != nil {
			os.Exit(10)
		}
		filter := unix.SockFprog{Len: uint16(len(program)), Filter: &program[0]}
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			os.Exit(11)
		}
		if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&filter)), 0, 0); err != nil {
			os.Exit(12)
		}
		status, err := os.ReadFile("/proc/self/status")
		if err != nil || !strings.Contains(string(status), "Seccomp:\t2\n") {
			os.Exit(13)
		}
		if unix.Getpid() <= 0 {
			os.Exit(14)
		}
		_, _, errno := unix.RawSyscall(unix.SYS_CLONE3, 0, 0, 0)
		if !errors.Is(errno, unix.ENOSYS) {
			os.Exit(15)
		}
		_, _, errno = unix.RawSyscall(unix.SYS_BPF, 0, 0, 0)
		if !errors.Is(errno, unix.EPERM) {
			os.Exit(16)
		}
		_, _, errno = unix.RawSyscall(unix.SYS_CLONE, uintptr(unix.CLONE_NEWUSER|unix.SIGCHLD), 0, 0)
		if !errors.Is(errno, unix.EPERM) {
			os.Exit(17)
		}
		if supported, err := querySeccompSupport(); err != nil || !supported {
			os.Exit(18)
		}
		os.Exit(0)
	}
	supported, err := querySeccompSupport()
	if err != nil {
		t.Fatal(err)
	}
	if !supported {
		t.Skip("seccomp filters are unsupported")
	}
	command := exec.Command(os.Args[0], "-test.run=^TestSeccompKernelEnforcement$")
	command.Env = append(os.Environ(), "BWRAP_AGENT_SECCOMP_TEST_HELPER=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("seccomp helper: %v: %s", err, output)
	}
}
