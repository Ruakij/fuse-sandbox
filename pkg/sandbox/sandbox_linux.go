//go:build linux

package sandbox

import (
	"encoding/json"
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
	spec, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("/proc/self/exe", initArg, string(spec))
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

// enter builds the root and execs the daemon in it. It only returns on failure.
func enter(cfg *Config) error {
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
	return syscall.Exec(cfg.Command[0], cfg.Command, os.Environ())
}
