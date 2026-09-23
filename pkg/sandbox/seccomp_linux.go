//go:build linux

package sandbox

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// denied are syscalls a FUSE daemon has no use for, and that open the kernel to
// it: mounting, namespaces, tracing, modules, keyrings, handles past the binds.
// Most need a capability the daemon lacks anyway; the filter is the second wall.
var denied = []uint32{
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT,
	unix.SYS_FSPICK, unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT, unix.SYS_MOUNT_SETATTR,
	unix.SYS_PIVOT_ROOT, unix.SYS_UNSHARE, unix.SYS_SETNS, unix.SYS_BPF, unix.SYS_PTRACE,
	unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_KEXEC_LOAD,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE, unix.SYS_PERF_EVENT_OPEN,
	unix.SYS_USERFAULTFD, unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
}

// clone creating a namespace is denied too. Its flags cannot be checked in
// clone3, which takes them in memory, so that reports ENOSYS and libc and Go
// fall back to clone.
const nsFlags = unix.CLONE_NEWNS | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWUSER |
	unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWCGROUP

var auditArch = map[string]uint32{
	"386": unix.AUDIT_ARCH_I386, "amd64": unix.AUDIT_ARCH_X86_64, "arm": unix.AUDIT_ARCH_ARM,
	"arm64": unix.AUDIT_ARCH_AARCH64, "ppc64le": unix.AUDIT_ARCH_PPC64LE,
	"riscv64": unix.AUDIT_ARCH_RISCV64, "s390x": unix.AUDIT_ARCH_S390X,
}

// seccompFilter builds the filter. Syscalls of any other ABI, which have other
// numbers, like 32-bit ones on amd64 and x32, kill the process.
func seccompFilter() ([]unix.SockFilter, error) {
	arch, ok := auditArch[runtime.GOARCH]
	if !ok {
		return nil, fmt.Errorf("no seccomp filter for %s", runtime.GOARCH)
	}
	const (
		ld  = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		jeq = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		ret = unix.BPF_RET | unix.BPF_K
		// Offsets into struct seccomp_data.
		nrOff, archOff, argsOff = 0, 4, 16
	)
	eperm := uint32(unix.SECCOMP_RET_ERRNO | unix.EPERM)
	f := []unix.SockFilter{
		{Code: ld, K: archOff},
		{Code: jeq, K: arch, Jt: 1},
		{Code: ret, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: ld, K: nrOff},
	}
	if runtime.GOARCH == "amd64" {
		const x32 = 0x40000000
		f = append(f,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, K: x32, Jf: 1},
			unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_KILL_PROCESS})
	}
	for _, nr := range denied {
		f = append(f, unix.SockFilter{Code: jeq, K: nr, Jf: 1}, unix.SockFilter{Code: ret, K: eperm})
	}
	// The flags are clone's first argument, but its second on s390x, and the low
	// 32 bits hold them all.
	flagsOff := uint32(argsOff)
	if runtime.GOARCH == "s390x" {
		flagsOff += 8 + 4
	}
	return append(f,
		unix.SockFilter{Code: jeq, K: unix.SYS_CLONE3, Jf: 1},
		unix.SockFilter{Code: ret, K: uint32(unix.SECCOMP_RET_ERRNO | unix.ENOSYS)},
		unix.SockFilter{Code: jeq, K: unix.SYS_CLONE, Jf: 3},
		unix.SockFilter{Code: ld, K: flagsOff},
		unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JSET | unix.BPF_K, K: nsFlags, Jf: 1},
		unix.SockFilter{Code: ret, K: eperm},
		unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW},
	), nil
}

// installSeccomp filters the calling thread and what it execs. It needs
// no_new_privs.
func installSeccomp() error {
	f, err := seccompFilter()
	if err != nil {
		return err
	}
	prog := unix.SockFprog{Len: uint16(len(f)), Filter: &f[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&prog)), 0, 0); err != nil {
		return fmt.Errorf("install seccomp filter: %w", err)
	}
	return nil
}
