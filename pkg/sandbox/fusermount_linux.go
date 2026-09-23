//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// fusermount stands in for fusermount in the sandbox: the supervisor mounts or
// unmounts, and a new mount's /dev/fuse fd goes back over _FUSE_COMMFD as
// fusermount would send it.
func fusermount(args []string) error {
	op, opts, mnt, err := fusermountArgs(args)
	if err != nil {
		return err
	}
	var comm int
	if op == opMount {
		if comm, err = strconv.Atoi(os.Getenv("_FUSE_COMMFD")); err != nil {
			return fmt.Errorf("_FUSE_COMMFD: %w", err)
		}
	}
	fd, err := unix.Open(mnt, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", mnt, err)
	}
	conn, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: sockPath, Net: "unixpacket"})
	if err != nil {
		return fmt.Errorf("reach the sandbox supervisor: %w", err)
	}
	defer conn.Close()
	if _, _, err := conn.WriteMsgUnix(append([]byte{op}, opts...), unix.UnixRights(fd), nil); err != nil {
		return err
	}
	buf, oob := make([]byte, 4096), make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return err
	}
	fds, err := rights(oob[:oobn])
	defer closeAll(fds)
	switch {
	case err != nil:
		return err
	case n == 0:
		return errors.New("no answer from the sandbox supervisor")
	case buf[0] != 0:
		return errors.New(string(buf[:n]))
	case op != opMount:
		return nil
	case len(fds) != 1:
		return errors.New("no /dev/fuse fd from the sandbox supervisor")
	}
	return unix.Sendmsg(comm, []byte{0}, unix.UnixRights(fds[0]), nil, 0)
}

// broker does the daemon's FUSE mounts: only on the target, only with the
// options parseMountOptions allows. Its requests come from inside the sandbox
// and are trusted no more than the daemon.
type broker struct {
	target string
	mu     sync.Mutex
	// mounted are the IDs of the mounts it made.
	mounted []uint64
}

func (b *broker) serve(l *net.UnixListener) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return
		}
		b.handle(c)
		c.Close()
	}
}

func (b *broker) handle(c *net.UnixConn) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	buf, oob := make([]byte, 4096), make([]byte, unix.CmsgSpace(4*4))
	n, oobn, flags, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return
	}
	fds, err := rights(oob[:oobn])
	defer closeAll(fds)
	fd := -1
	switch {
	case err != nil || n == 0 || len(fds) != 1 || flags&unix.MSG_CTRUNC != 0:
		err = errors.New("malformed request to the sandbox supervisor")
	default:
		fd, err = b.do(buf[0], string(buf[1:n]), fds[0])
	}
	if err != nil {
		_, _ = c.Write([]byte(err.Error()))
		return
	}
	var reply []byte
	if fd >= 0 {
		defer unix.Close(fd)
		reply = unix.UnixRights(fd)
	}
	_, _, _ = c.WriteMsgUnix([]byte{0}, reply, nil)
}

// do carries out a request, and returns the /dev/fuse fd of a new mount.
func (b *broker) do(op byte, opts string, mountpoint int) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	top, _, err := mountAt(unix.AT_FDCWD, b.target)
	if err != nil {
		return -1, err
	}
	if id, root, err := mountAt(mountpoint, ""); err != nil || id != top || !root {
		return -1, fmt.Errorf("only %s may be mounted in the sandbox", b.target)
	}
	switch op {
	case opMount:
		m, err := parseMountOptions(opts)
		if err != nil {
			return -1, err
		}
		return b.mount(m)
	case opUnmount, opUnmountLazy:
		if !slices.Contains(b.mounted, top) {
			return -1, fmt.Errorf("%s was not mounted through fusermount", b.target)
		}
		flags := 0
		if op == opUnmountLazy {
			flags = unix.MNT_DETACH
		}
		return -1, unix.Unmount(b.target, flags)
	}
	return -1, errors.New("unknown request to the sandbox supervisor")
}

func (b *broker) mount(m fuseMount) (int, error) {
	var st unix.Stat_t
	if err := unix.Stat(b.target, &st); err != nil {
		return -1, err
	}
	fd, err := unix.Open("/dev/fuse", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open /dev/fuse: %w", err)
	}
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV)
	if m.readOnly {
		flags |= unix.MS_RDONLY
	}
	if m.noexec {
		flags |= unix.MS_NOEXEC
	}
	if m.noatime {
		flags |= unix.MS_NOATIME
	}
	fstype, source := "fuse", "/dev/fuse"
	if m.subtype != "" {
		fstype, source = fstype+"."+m.subtype, m.subtype
	}
	if m.source != "" {
		source = m.source
	}
	data := append([]string{"fd=" + strconv.Itoa(fd), fmt.Sprintf("rootmode=%o", st.Mode&unix.S_IFMT), "user_id=0", "group_id=0"}, m.opts...)
	if err := unix.Mount(source, b.target, fstype, flags, strings.Join(data, ",")); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("mount %s: %w", b.target, err)
	}
	if id, _, err := mountAt(unix.AT_FDCWD, b.target); err == nil {
		b.mounted = append(b.mounted, id)
	}
	return fd, nil
}

// unmountOwn detaches the mounts it made that are still on the target, as the
// daemon, lacking the capability, cannot on its way out.
func (b *broker) unmountOwn() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for {
		id, _, err := mountAt(unix.AT_FDCWD, b.target)
		if err != nil || !slices.Contains(b.mounted, id) || unix.Unmount(b.target, unix.MNT_DETACH) != nil {
			return
		}
	}
}

// mountAt returns the ID of the mount path is on, and whether path is its root.
// It does not ask a FUSE daemon, which may be the one asking.
func mountAt(dirfd int, path string) (id uint64, root bool, err error) {
	flags := unix.AT_SYMLINK_NOFOLLOW | unix.AT_STATX_DONT_SYNC
	if path == "" {
		flags |= unix.AT_EMPTY_PATH
	}
	var st unix.Statx_t
	if err := unix.Statx(dirfd, path, flags, unix.STATX_MNT_ID, &st); err != nil {
		return 0, false, err
	}
	return st.Mnt_id, st.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0, nil
}

// rights returns the fds passed in oob, all of them so they can be closed.
func rights(oob []byte) ([]int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	var fds []int
	for _, m := range msgs {
		f, ferr := unix.ParseUnixRights(&m)
		fds = append(fds, f...)
		err = errors.Join(err, ferr)
	}
	return fds, err
}

func closeAll(fds []int) {
	for _, fd := range fds {
		unix.Close(fd)
	}
}
