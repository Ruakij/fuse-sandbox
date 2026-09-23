//go:build linux

package sandbox

import (
	"debug/elf"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Ruakij/fuse-sandbox/internal/rootfs"
)

// Command returns a command that runs cfg's daemon in new mount, PID, IPC,
// UTS, cgroup and, unless cfg.ShareNet, network namespaces, under a supervisor
// that is PID 1. The caller sets its I/O and starts it; the daemon, and with it
// the command, exits when its mount on the target is unmounted.
func Command(cfg *Config) (*exec.Cmd, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// Both run in the sandbox, this program as the fusermount stand-in.
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	for _, bin := range []string{self, cfg.Command[0]} {
		if err := checkStatic(bin); err != nil {
			return nil, err
		}
	}
	noEnv := *cfg
	noEnv.Env = nil
	spec, err := encodeSpec(&noEnv)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("/proc/self/exe", initArg, spec)
	cmd.Env = cfg.Env
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

// keptCaps are what a FUSE daemon serving files as root uses: acting on files
// for callers of any uid. Not CAP_SYS_ADMIN, the supervisor mounts for it, nor
// CAP_DAC_READ_SEARCH: DAC_OVERRIDE covers reading, and with it
// open_by_handle_at reaches any inode on a bound filesystem, outside the bind.
var keptCaps = []int{
	unix.CAP_CHOWN, unix.CAP_DAC_OVERRIDE, unix.CAP_FOWNER, unix.CAP_FSETID,
	unix.CAP_SETUID, unix.CAP_SETGID, unix.CAP_SETFCAP, unix.CAP_LINUX_IMMUTABLE,
	unix.CAP_LEASE, unix.CAP_SYS_RESOURCE, unix.CAP_SYS_NICE,
}

// supervise builds the root and stays in it as PID 1 with all capabilities: it
// starts the daemon, mounts for it, reaps what it leaves behind, forwards
// signals and exits with its status. It only returns on failure.
func supervise(cfg *Config) error {
	if err := closeInherited(); err != nil {
		return err
	}
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
	const exe = unix.MOUNT_ATTR_RDONLY | unix.MOUNT_ATTR_NOSUID | unix.MOUNT_ATTR_NODEV
	mounts = append(mounts, rootfs.Mount{Host: cfg.Command[0], Dst: cfg.Command[0], Attr: exe})
	// By path: /proc/self/exe leads to the host's mount namespace, which the
	// mount cannot be cloned from.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	for _, f := range cfg.fusermount() {
		mounts = append(mounts, rootfs.Mount{Host: self, Dst: f, Attr: exe})
	}
	if err := rootfs.Enter(cfg.Target.Host, cfg.Target.Sandbox, mounts); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o755); err != nil {
		return err
	}
	l, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: sockPath, Net: "unixpacket"})
	if err != nil {
		return err
	}
	if err := rootfs.Seal(); err != nil {
		return err
	}

	spec, err := encodeSpec(cfg)
	if err != nil {
		return err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, unix.SIGTERM, unix.SIGINT, unix.SIGHUP)
	daemon, err := os.StartProcess(cfg.fusermount()[0], []string{"fuse-sandbox", daemonArg, spec},
		&os.ProcAttr{Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	if err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	go func() {
		for s := range sigs {
			_ = daemon.Signal(s)
		}
	}()
	b := &broker{target: cfg.Target.Sandbox}
	go b.serve(l)

	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait for daemon: %w", err)
		}
		if pid != daemon.Pid {
			continue
		}
		b.unmountOwn()
		if ws.Signaled() {
			os.Exit(128 + int(ws.Signal()))
		}
		os.Exit(ws.ExitStatus())
	}
}

// closeInherited closes the fds beyond stdio this process was started with,
// which, unlike Go's own, lack close-on-exec. Go would pass them on to the
// daemon, and through /proc/self/fd each would lead out of the sandbox.
func closeInherited() error {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return fmt.Errorf("close inherited fds: %w", err)
	}
	for _, e := range ents {
		fd, _ := strconv.Atoi(e.Name())
		if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); fd > 2 && err == nil && flags&unix.FD_CLOEXEC == 0 {
			unix.Close(fd)
		}
	}
	return nil
}

// execDaemon runs in the sandbox, started by the supervisor, and becomes the
// daemon. It only returns on failure.
func execDaemon(cfg *Config) error {
	// Capabilities and no_new_privs are per thread, and exec takes them from the
	// calling one.
	runtime.LockOSThread()
	if err := rootfs.AttachProcFD(); err != nil {
		return err
	}
	if err := dropPrivileges(); err != nil {
		return err
	}
	return syscall.Exec(cfg.Command[0], cfg.Command, os.Environ())
}

// checkStatic fails for a dynamically linked program: the sandbox holds no
// loader or libraries.
func checkStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is dynamically linked, but runs in the sandbox without its loader", path)
		}
	}
	return nil
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
