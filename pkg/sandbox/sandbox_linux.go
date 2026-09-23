//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Ruakij/fuse-sandbox/internal/rootfs"
)

// Command returns a command that runs cfg's daemon as PID 1 of new mount, PID,
// IPC, UTS, cgroup and, unless cfg.ShareNet, network namespaces. The caller sets its I/O and starts it; the daemon, and with it
// the command, exits when its mount on the target is unmounted.
func Command(cfg *Config) (*exec.Cmd, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	spec, err := encodeSpec(cfg)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("/proc/self/exe", initArg, spec)
	flags := uintptr(unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWIPC | unix.CLONE_NEWUTS | unix.CLONE_NEWCGROUP)
	if !cfg.ShareNet {
		flags |= unix.CLONE_NEWNET
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags}
	return cmd, nil
}

// Run runs the sandbox in the foreground with the caller's stdio, forwarding
// SIGTERM, SIGINT and SIGHUP and killing it if the caller dies. It returns the
// daemon's exit code, or 128 plus the signal that ended it.
func Run(cfg *Config) (int, error) {
	cmd, err := Command(cfg)
	if err != nil {
		return 0, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Pdeathsig fires when the thread that started the child exits, not the process.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	cmd.SysProcAttr.Pdeathsig = unix.SIGKILL
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start sandbox: %w", err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, unix.SIGTERM, unix.SIGINT, unix.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			_ = cmd.Process.Signal(s)
		}
	}()

	err = cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exit):
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return exit.ExitCode(), nil
	default:
		return 0, err
	}
}

// keptCaps are what a FUSE daemon serving files as root uses: mounting, and
// acting on files for callers of any uid.
var keptCaps = []int{
	unix.CAP_SYS_ADMIN, unix.CAP_CHOWN, unix.CAP_DAC_OVERRIDE, unix.CAP_DAC_READ_SEARCH,
	unix.CAP_FOWNER, unix.CAP_FSETID, unix.CAP_SETUID, unix.CAP_SETGID, unix.CAP_SETFCAP,
	unix.CAP_MKNOD, unix.CAP_LINUX_IMMUTABLE, unix.CAP_LEASE, unix.CAP_SYS_RESOURCE, unix.CAP_SYS_NICE,
}

// enter builds the root and execs the daemon in it. It only returns on failure.
func enter(cfg *Config) error {
	// Capabilities and no_new_privs are per thread, and exec takes them from the
	// calling one.
	runtime.LockOSThread()
	var mounts []rootfs.Mount
	for _, b := range cfg.Binds {
		attr := uint64(unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV | unix.MOUNT_ATTR_NOEXEC)
		if b.ReadOnly {
			attr |= unix.MOUNT_ATTR_RDONLY
		}
		mounts = append(mounts, rootfs.Mount{Host: b.Host, Dst: b.Sandbox, Recursive: true, Attr: attr})
	}
	for _, d := range append(cfg.Devices, defaultDevices...) {
		mounts = append(mounts, rootfs.Mount{Host: d, Dst: d, Attr: unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NOEXEC, Device: true})
	}
	mounts = append(mounts, rootfs.Mount{
		Host: cfg.Command[0], Dst: cfg.Command[0],
		Attr: unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV,
	})

	if err := rootfs.Enter(cfg.Target.Host, cfg.Target.Sandbox, mounts); err != nil {
		return err
	}
	if err := dropPrivileges(); err != nil {
		return err
	}
	// Go passes on the fds a process inherits, and through /proc/self/fd each
	// would lead the daemon out of the sandbox.
	if err := unix.CloseRange(3, ^uint(0), unix.CLOSE_RANGE_CLOEXEC); err != nil {
		return fmt.Errorf("close inherited fds: %w", err)
	}
	return syscall.Exec(cfg.Command[0], cfg.Command, os.Environ())
}

// dropPrivileges bounds the daemon to keptCaps, which root gets all of on exec,
// and keeps it from gaining any more through a later exec.
func dropPrivileges() error {
	var keep uint64
	for _, c := range keptCaps {
		keep |= 1 << c
	}
	for c := 0; c < 64; c++ {
		if keep&(1<<c) != 0 {
			continue
		}
		err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0)
		if errors.Is(err, unix.EINVAL) {
			break // past the kernel's last capability
		}
		if err != nil {
			return fmt.Errorf("drop capability %d: %w", c, err)
		}
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("capget: %w", err)
	}
	data[0].Inheritable, data[1].Inheritable = 0, 0
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("clear inheritable capabilities: %w", err)
	}
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}
