//go:build linux

package sandbox

import (
	"errors"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSeccompFilter(t *testing.T) {
	type result struct {
		name string
		err  error
		want error
	}
	results := make(chan []result)
	// The filter is per thread, and a locked thread its goroutine exits in is
	// thrown away, filter and all.
	go func() {
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			results <- []result{{"no_new_privs", err, nil}}
			return
		}
		if err := installSeccomp(); err != nil {
			results <- []result{{"install", err, nil}}
			return
		}
		errno := func(nr, a0, a1 uintptr) error {
			if _, _, e := unix.RawSyscall(nr, a0, a1, 0); e != 0 {
				return e
			}
			return nil
		}
		results <- []result{
			{"unshare", errno(unix.SYS_UNSHARE, unix.CLONE_NEWUSER, 0), unix.EPERM},
			{"open_by_handle_at", errno(unix.SYS_OPEN_BY_HANDLE_AT, ^uintptr(0), 0), unix.EPERM},
			{"mount", errno(unix.SYS_MOUNT, 0, 0), unix.EPERM},
			{"clone3", errno(unix.SYS_CLONE3, 0, 0), unix.ENOSYS},
			// Invalid past the filter, so the kernel would not fork.
			{"clone with a namespace", errno(unix.SYS_CLONE, unix.CLONE_NEWUSER|unix.CLONE_FS, 0), unix.EPERM},
			{"clone without one", errno(unix.SYS_CLONE, unix.CLONE_SIGHAND, 0), unix.EINVAL},
			{"getpid", errno(unix.SYS_GETPID, 0, 0), nil},
		}
	}()
	for _, r := range <-results {
		if !errors.Is(r.err, r.want) {
			t.Errorf("%s: %v, want %v", r.name, r.err, r.want)
		}
	}
}
