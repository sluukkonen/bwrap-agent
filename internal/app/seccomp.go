//go:build linux

package app

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	seccompProfileDevelopment = "development"
	seccompProfilePodman      = "podman"
	seccompDataNumberOffset   = 0
	seccompDataArchOffset     = 4
	seccompDataArgsOffset     = 16
)

const seccompNamespaceCloneFlags = uint32(
	unix.CLONE_NEWTIME |
		unix.CLONE_NEWNS |
		unix.CLONE_NEWCGROUP |
		unix.CLONE_NEWUTS |
		unix.CLONE_NEWIPC |
		unix.CLONE_NEWUSER |
		unix.CLONE_NEWPID |
		unix.CLONE_NEWNET)

type seccompArchitecture struct {
	supported      bool
	audit          uint32
	compatMask     uint32
	syscalls       map[string]uint32
	commonSyscalls []string
}

type seccompRule struct {
	name  string
	errno unix.Errno
}

var commonSeccompRules = []seccompRule{
	{name: "acct", errno: unix.EPERM},
	{name: "delete_module", errno: unix.EPERM},
	{name: "finit_module", errno: unix.EPERM},
	{name: "init_module", errno: unix.EPERM},
	{name: "kexec_file_load", errno: unix.EPERM},
	{name: "kexec_load", errno: unix.EPERM},
	{name: "lookup_dcookie", errno: unix.EPERM},
	{name: "swapoff", errno: unix.EPERM},
	{name: "swapon", errno: unix.EPERM},
	{name: "syslog", errno: unix.EPERM},
}

var developmentSeccompRules = []seccompRule{
	{name: "bpf", errno: unix.EPERM},
	{name: "clock_settime", errno: unix.EPERM},
	{name: "settimeofday", errno: unix.EPERM},
	{name: "reboot", errno: unix.EPERM},
	{name: "add_key", errno: unix.EPERM},
	{name: "keyctl", errno: unix.EPERM},
	{name: "request_key", errno: unix.EPERM},
	{name: "quotactl", errno: unix.EPERM},
	{name: "quotactl_fd", errno: unix.EPERM},
	{name: "name_to_handle_at", errno: unix.EPERM},
	{name: "open_by_handle_at", errno: unix.EPERM},
	{name: "chroot", errno: unix.EPERM},
	{name: "mount", errno: unix.EPERM},
	{name: "umount2", errno: unix.EPERM},
	{name: "pivot_root", errno: unix.EPERM},
	{name: "setns", errno: unix.EPERM},
	{name: "unshare", errno: unix.EPERM},
	{name: "open_tree", errno: unix.ENOSYS},
	{name: "move_mount", errno: unix.ENOSYS},
	{name: "fsopen", errno: unix.ENOSYS},
	{name: "fsconfig", errno: unix.ENOSYS},
	{name: "fsmount", errno: unix.ENOSYS},
	{name: "fspick", errno: unix.ENOSYS},
	{name: "mount_setattr", errno: unix.ENOSYS},
	{name: "clone3", errno: unix.ENOSYS},
}

func resolveSeccompRules(profile string) ([]seccompRule, error) {
	rules := append([]seccompRule{}, commonSeccompRules...)
	for _, name := range nativeSeccompArchitecture.commonSyscalls {
		rules = append(rules, seccompRule{name: name, errno: unix.EPERM})
	}
	switch profile {
	case seccompProfileDevelopment:
		return append(rules, developmentSeccompRules...), nil
	case seccompProfilePodman:
		return rules, nil
	default:
		return nil, fmt.Errorf("unknown seccomp profile %q", profile)
	}
}

func determineSeccompStatus(mode string, podmanEnabled bool, query func() (bool, error)) (SeccompStatus, string, error) {
	status := SeccompStatus{Requested: mode}
	if mode == "off" {
		status.Effective = "off"
		return status, "", nil
	}
	if !nativeSeccompArchitecture.supported {
		message := "seccomp filtering is unavailable on this architecture"
		if mode == "required" {
			return SeccompStatus{}, "", errors.New(message)
		}
		status.Effective = "unsupported"
		return status, message, nil
	}
	supported, err := query()
	if err != nil || !supported {
		message := "seccomp filtering is unavailable"
		if err != nil {
			message = fmt.Sprintf("query seccomp support: %v", err)
		}
		if mode == "required" {
			return SeccompStatus{}, "", errors.New(message)
		}
		status.Effective = "unsupported"
		return status, message, nil
	}
	status.Effective = "enabled"
	if podmanEnabled {
		status.Profile = seccompProfilePodman
	} else {
		status.Profile = seccompProfileDevelopment
	}
	return status, "", nil
}

func querySeccompSupport() (bool, error) {
	if !nativeSeccompArchitecture.supported {
		return false, nil
	}
	for _, value := range []uint32{unix.SECCOMP_RET_ERRNO, unix.SECCOMP_RET_KILL_PROCESS} {
		action := value
		_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_GET_ACTION_AVAIL, 0,
			uintptr(unsafe.Pointer(&action)))
		if errno == 0 {
			continue
		}
		if errors.Is(errno, unix.ENOSYS) || errors.Is(errno, unix.EINVAL) || errors.Is(errno, unix.EOPNOTSUPP) {
			return false, nil
		}
		return false, errno
	}
	return true, nil
}

func seccompStatement(code uint16, value uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: value}
}

func seccompJump(code uint16, value uint32, onTrue, onFalse uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: onTrue, Jf: onFalse, K: value}
}

func seccompErrno(errno unix.Errno) uint32 {
	return unix.SECCOMP_RET_ERRNO | uint32(errno)
}

func compileSeccompFilter(profile string) ([]unix.SockFilter, error) {
	if !nativeSeccompArchitecture.supported {
		return nil, errors.New("seccomp filtering is unavailable on this architecture")
	}
	rules, err := resolveSeccompRules(profile)
	if err != nil {
		return nil, err
	}
	program := []unix.SockFilter{
		seccompStatement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArchOffset),
		seccompJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, nativeSeccompArchitecture.audit, 1, 0),
		seccompStatement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		seccompStatement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataNumberOffset),
	}
	if nativeSeccompArchitecture.compatMask != 0 {
		program = append(program,
			seccompJump(unix.BPF_JMP|unix.BPF_JSET|unix.BPF_K, nativeSeccompArchitecture.compatMask, 0, 1),
			seccompStatement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS))
	}
	seen := map[uint32]string{}
	for _, rule := range rules {
		number, found := nativeSeccompArchitecture.syscalls[rule.name]
		if !found {
			return nil, fmt.Errorf("seccomp syscall %q is unavailable on this architecture", rule.name)
		}
		if previous, duplicate := seen[number]; duplicate {
			return nil, fmt.Errorf("seccomp syscalls %q and %q share number %d", previous, rule.name, number)
		}
		seen[number] = rule.name
		program = append(program,
			seccompJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, number, 0, 1),
			seccompStatement(unix.BPF_RET|unix.BPF_K, seccompErrno(rule.errno)))
	}
	if profile == seccompProfileDevelopment {
		clone, found := nativeSeccompArchitecture.syscalls["clone"]
		if !found {
			return nil, errors.New("seccomp clone syscall is unavailable on this architecture")
		}
		program = append(program,
			seccompJump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, clone, 0, 3),
			seccompStatement(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArgsOffset),
			seccompJump(unix.BPF_JMP|unix.BPF_JSET|unix.BPF_K, seccompNamespaceCloneFlags, 0, 1),
			seccompStatement(unix.BPF_RET|unix.BPF_K, seccompErrno(unix.EPERM)))
	}
	program = append(program, seccompStatement(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
	return program, nil
}

func marshalSeccompFilter(program []unix.SockFilter) ([]byte, error) {
	if len(program) == 0 || len(program) > 0xffff {
		return nil, fmt.Errorf("invalid seccomp filter length %d", len(program))
	}
	content := make([]byte, len(program)*8)
	for index, instruction := range program {
		offset := index * 8
		binary.LittleEndian.PutUint16(content[offset:], instruction.Code)
		content[offset+2] = instruction.Jt
		content[offset+3] = instruction.Jf
		binary.LittleEndian.PutUint32(content[offset+4:], instruction.K)
	}
	return content, nil
}

func openSeccompFilter(profile string) (*os.File, error) {
	program, err := compileSeccompFilter(profile)
	if err != nil {
		return nil, err
	}
	content, err := marshalSeccompFilter(program)
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(writer, bytes.NewReader(content)); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return reader, nil
}
