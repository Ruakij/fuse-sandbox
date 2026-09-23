//go:build linux && mounttest

package sandbox

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// FuzzSandbox builds real sandboxes from fuzzed layouts: the target's sandbox
// path, and newline-separated sandbox paths, each bound from the host path its
// pick selects, read-only if the pick's high bit is set. The daemon must see
// exactly what is bound, submounts included, and never the secret beside it, and
// a refused host path must leave nothing mounted.
func FuzzSandbox(f *testing.F) {
	f.Add("/t", "/data", []byte{0})
	f.Add("/x/t", "/data\n/x/file\n/dev/sub", []byte{0x80, 1, 0x82})
	f.Add("/t", "/a\n/b", []byte{0, 3})
	f.Add("/t", "/a", []byte{4})
	f.Add("/t", "/a", []byte{5})
	f.Add("/t", "/a", []byte{6})

	f.Fuzz(func(t *testing.T, target, dsts string, picks []byte) {
		e := newEnv(t)
		sub := filepath.Join(e.data, "sub")
		must(t, unix.Mount("sub", sub, "tmpfs", 0, ""))
		t.Cleanup(func() { _ = unix.Unmount(sub, unix.MNT_DETACH) })
		must(t, os.WriteFile(filepath.Join(sub, "m"), []byte("m"), 0o644))
		link := filepath.Join(e.root, "link")
		must(t, os.Symlink(e.secret, link))
		hosts := []struct {
			path    string
			refused bool
		}{
			{e.data, false},
			{filepath.Join(e.data, "file"), false},
			{sub, false},
			{link, true},
			{filepath.Join(link, "shadow"), true},
			{"/dev/null", true},
			{filepath.Join(e.root, "missing"), true},
		}

		cfg := &Config{
			Target:  Bind{Host: e.target, Sandbox: target},
			Devices: []string{"/dev/fuse"},
			Command: []string{loopfsBin, "/", target},
		}
		split := strings.Split(dsts, "\n")
		if len(split) > 4 {
			return
		}
		refused := false
		for i, dst := range split {
			var pick byte
			if i < len(picks) {
				pick = picks[i]
			}
			h := hosts[int(pick&0x7f)%len(hosts)]
			refused = refused || h.refused
			cfg.Binds = append(cfg.Binds, Bind{Host: h.path, Sandbox: dst, ReadOnly: pick&0x80 != 0})
		}
		cmd, err := Command(cfg)
		if err != nil {
			return
		}

		// Where a mount leaking out of the sandbox would land: on a host path it
		// takes in. Other fuzz workers mount on none of these.
		watched := []string{"/", "/dev", "/dev/null", "/dev/fuse", filepath.Dir(loopfsBin), loopfsBin,
			e.root, e.target, e.data, filepath.Join(e.data, "file"), sub, e.secret, link}
		before := mountIDs(t, watched)
		if refused {
			wantRefusal(t, cmd, e.target)
		} else {
			startMounted(t, cmd, e.target)
		}
		for p, id := range mountIDs(t, watched) {
			if moved := id != before[p]; moved != (p == e.target && !refused) {
				t.Fatalf("mount on %s changed: %v", p, moved)
			}
		}
		if refused {
			return
		}

		want := []string{"dev", "proc", strings.Split(target, "/")[1], strings.Split(loopfsBin, "/")[1]}
		for _, b := range cfg.Binds {
			want = append(want, strings.Split(b.Sandbox, "/")[1])
		}
		slices.Sort(want)
		if got := names(t, e.target); !slices.Equal(got, slices.Compact(want)) {
			t.Fatalf("sandbox root = %q, want %q", got, slices.Compact(want))
		}

		// What each host path holds, and its directories, submounts included.
		type holds struct {
			files map[string]string
			dirs  []string
		}
		content := map[string]holds{
			e.data:                        {map[string]string{"file": "data", "sub/m": "m"}, []string{"", "sub"}},
			filepath.Join(e.data, "file"): {map[string]string{"": "data"}, nil},
			sub:                           {map[string]string{"m": "m"}, []string{""}},
		}
		for _, b := range cfg.Binds {
			p := filepath.Join(e.target, b.Sandbox)
			for rel, data := range content[b.Host].files {
				wantContent(t, filepath.Join(p, rel), data)
			}
			for _, dir := range content[b.Host].dirs {
				w := filepath.Join(p, dir, "w")
				err := os.WriteFile(w, nil, 0o644)
				if b.ReadOnly != errors.Is(err, unix.EROFS) {
					t.Errorf("write to %s, read-only %v: %v", filepath.Join(b.Sandbox, dir), b.ReadOnly, err)
				}
				_ = os.Remove(w)
			}
		}

		err = filepath.WalkDir(e.target, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			// The target is the daemon's own mount, and proc holds only its fds.
			if p == filepath.Join(e.target, target) || p == filepath.Join(e.target, "proc") {
				return fs.SkipDir
			}
			if d.Name() == "shadow" {
				t.Errorf("secret reachable at %s", p)
			}
			return nil
		})
		must(t, err)
	})
}

// mountIDs maps each path to the unique ID of the mount it is on.
func mountIDs(t *testing.T, paths []string) map[string]uint64 {
	t.Helper()
	ids := map[string]uint64{}
	for _, p := range paths {
		var st unix.Statx_t
		must(t, unix.Statx(unix.AT_FDCWD, p, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID_UNIQUE, &st))
		if st.Mask&unix.STATX_MNT_ID_UNIQUE == 0 {
			t.Skip("kernel has no unique mount IDs")
		}
		ids[p] = st.Mnt_id
	}
	return ids
}

// wantRefusal runs a sandbox that must fail before mounting its target.
func wantRefusal(t *testing.T, cmd *exec.Cmd, target string) {
	t.Helper()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	startDying(t, cmd)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("sandbox with a refused bind exited cleanly\n%s", &out)
		}
	case <-time.After(10 * time.Second):
		_ = unix.Unmount(target, unix.MNT_DETACH)
		_ = cmd.Process.Kill()
		t.Fatalf("sandbox with a refused bind still running after 10s\n%s", &out)
	}
	if isFUSE(target) {
		t.Fatal("target mounted despite a refused bind")
	}
}
