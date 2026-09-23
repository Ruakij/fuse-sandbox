//go:build linux

package rootfs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Mount is a host path cloned into the new root.
type Mount struct {
	Host, Dst string
	// Recursive takes the submounts along.
	Recursive bool
	// Attr is a set of unix.MOUNT_ATTR_* flags.
	Attr uint64
	// Device requires Host to be a character device, and forbids one otherwise.
	Device bool
}

type attach struct {
	fd  int
	dst string
}

// Enter pivots the calling process, which must be alone in its mount namespace,
// into a new read-only root that holds the target, the mounts and the caller's
// /proc/self/fd. Only the target keeps propagating to the host.
func Enter(targetHost, targetDst string, mounts []Mount) error {
	// Cloned before propagation is cut, so it stays a peer of the host's mount and
	// whatever gets mounted on it propagates out.
	target, err := cloneTree(targetHost, false)
	if err != nil {
		return err
	}
	// Host mounts still propagate in, so recursive mounts pick up later
	// submounts, but nothing mounted in here propagates out.
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("make / rslave: %w", err)
	}

	attached := []attach{{target, targetDst}}
	for _, m := range mounts {
		fd, err := clone(m)
		if err != nil {
			return err
		}
		attached = append(attached, attach{fd, m.Dst})
	}
	return enterNewRoot(attached)
}

func clone(m Mount) (int, error) {
	fd, err := cloneTree(m.Host, m.Recursive)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return -1, fmt.Errorf("stat %s: %w", m.Host, err)
	}
	if isDev := st.Mode&unix.S_IFMT == unix.S_IFCHR; isDev != m.Device {
		return -1, fmt.Errorf("%s: character device is %v, want %v", m.Host, isDev, m.Device)
	}
	flags := unix.AT_EMPTY_PATH
	if m.Recursive {
		flags |= unix.AT_RECURSIVE
	}
	if err := unix.MountSetattr(fd, "", uint(flags), &unix.MountAttr{Attr_set: m.Attr}); err != nil {
		return -1, fmt.Errorf("set mount attributes on %s: %w", m.Host, err)
	}
	return fd, nil
}

// cloneTree opens host refusing any symlink on the way, and clones the mount at
// that exact inode, so nothing swapped in after the caller checked the path can
// end up in the sandbox.
func cloneTree(host string, recursive bool) (int, error) {
	fd, err := unix.Openat2(unix.AT_FDCWD, host, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS,
	})
	if errors.Is(err, unix.ELOOP) {
		return -1, fmt.Errorf("%s contains a symlink, pass the resolved path", host)
	}
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", host, err)
	}
	defer unix.Close(fd)

	flags := unix.OPEN_TREE_CLONE | unix.OPEN_TREE_CLOEXEC | unix.AT_EMPTY_PATH
	if recursive {
		flags |= unix.AT_RECURSIVE
	}
	tree, err := unix.OpenTree(fd, "", uint(flags))
	if err != nil {
		return -1, fmt.Errorf("clone %s: %w", host, err)
	}
	return tree, nil
}

func newFS(fstype string, opts map[string]string, attr int) (int, error) {
	fsfd, err := unix.Fsopen(fstype, unix.FSOPEN_CLOEXEC)
	if err != nil {
		return -1, fmt.Errorf("fsopen %s: %w", fstype, err)
	}
	defer unix.Close(fsfd)
	for k, v := range opts {
		if err := unix.FsconfigSetString(fsfd, k, v); err != nil {
			return -1, fmt.Errorf("%s %s=%s: %w", fstype, k, v, err)
		}
	}
	if err := unix.FsconfigCreate(fsfd); err != nil {
		return -1, fmt.Errorf("create %s: %w", fstype, err)
	}
	fd, err := unix.Fsmount(fsfd, unix.FSMOUNT_CLOEXEC, attr)
	if err != nil {
		return -1, fmt.Errorf("fsmount %s: %w", fstype, err)
	}
	return fd, nil
}

// enterNewRoot stacks an empty tmpfs on / and pivots into it with the mounts
// attached. Stacking needs no existing directory, which a read-only scratch
// image may not have.
func enterNewRoot(mounts []attach) error {
	root, err := newFS("tmpfs", map[string]string{"mode": "0755"}, unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV)
	if err != nil {
		return err
	}
	if err := unix.MoveMount(root, "", unix.AT_FDCWD, "/", unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("attach new root: %w", err)
	}
	if err := unix.Fchdir(root); err != nil {
		return fmt.Errorf("enter new root: %w", err)
	}

	if err := attachProcFD(); err != nil {
		return err
	}
	for _, m := range mounts {
		if err := attachAt(m.fd, m.dst[1:]); err != nil {
			return err
		}
	}

	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}
	return unix.MountSetattr(unix.AT_FDCWD, "/", 0, &unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY})
}

// attachProcFD gives the new root /proc/self/fd, which daemons like mergerfs
// need to reopen their own files, and nothing else of proc: through the rest, a
// daemon tricked into following a symlink would read its own memory and
// environment, or the host paths in its mountinfo. Must run as PID 1 of a new PID
// namespace, which exec keeps, so self stays the daemon.
func attachProcFD() error {
	proc, err := newFS("proc", map[string]string{"subset": "pid"},
		unix.MOUNT_ATTR_RDONLY|unix.MOUNT_ATTR_NOSUID|unix.MOUNT_ATTR_NODEV|unix.MOUNT_ATTR_NOEXEC)
	if err != nil {
		return err
	}
	// Attached for a moment, as kernels before 6.15 clone only from mounts in
	// the caller's namespace.
	const tmp = "proc-full"
	if err := attachAt(proc, tmp); err != nil {
		return err
	}
	fd, err := unix.OpenTree(unix.AT_FDCWD, tmp+"/self/fd", unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("clone /proc/self/fd: %w", err)
	}
	if err := unix.Unmount(tmp, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("detach proc: %w", err)
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	return attachAt(fd, "proc/self/fd")
}

// attachAt creates a mountpoint of the tree's type at rel, relative to the new
// root, and moves the tree onto it.
func attachAt(fd int, rel string) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		err := os.Mkdir(rel, 0o755)
		if err != nil {
			return err
		}
	} else {
		f, err := os.OpenFile(rel, os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		f.Close()
	}
	if err := unix.MoveMount(fd, "", unix.AT_FDCWD, rel, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("attach /%s: %w", rel, err)
	}
	return unix.Close(fd)
}
