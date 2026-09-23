//go:build linux && mounttest

// Real sandboxes around a real FUSE daemon. Needs root, a Linux kernel and
// /dev/fuse, so it is behind the mounttest build tag: see "make test-mount".
//
// The daemon is loopfs serving the sandbox's own root at the target, so reading
// the target from the host shows exactly what a daemon in the sandbox can reach.
package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const fuseSuperMagic = 0x65735546

var sandboxBin, loopfsBin string

func TestMain(m *testing.M) {
	Init()
	dir, err := os.MkdirTemp("", "fuse-sandbox-bin")
	if err != nil {
		panic(err)
	}
	sandboxBin, loopfsBin = filepath.Join(dir, "fuse-sandbox"), filepath.Join(dir, "loopfs")
	for bin, pkg := range map[string]string{sandboxBin: "../../cmd/fuse-sandbox", loopfsBin: "../../test/loopfs"} {
		cmd := exec.Command("go", "build", "-o", bin, pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			panic(string(out))
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	root, target, data, secret string
}

// newEnv lays out a target and a data dir on a shared mount, as a kubelet pods
// dir and a hostPath would be.
func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	must(t, unix.Mount(root, root, "", unix.MS_BIND, ""))
	t.Cleanup(func() { _ = unix.Unmount(root, unix.MNT_DETACH) })
	must(t, unix.Mount("", root, "", unix.MS_REC|unix.MS_SHARED, ""))

	e := &env{root: root, target: filepath.Join(root, "target"), data: filepath.Join(root, "data"), secret: filepath.Join(root, "secret")}
	for _, d := range []string{e.target, e.data, e.secret, filepath.Join(e.data, "sub")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	must(t, os.WriteFile(filepath.Join(e.data, "file"), []byte("data"), 0o644))
	must(t, os.WriteFile(filepath.Join(e.secret, "shadow"), []byte("secret"), 0o600))
	return e
}

func (e *env) args(extra ...string) []string {
	args := append([]string{"-target", e.target + ":/t", "-dev", "/dev/fuse"}, extra...)
	return append(args, "--", loopfsBin, "/", "/t")
}

// start runs the sandbox and returns once its mount is live at the target, and a
// channel closed on its exit.
func (e *env) start(t *testing.T, extra ...string) <-chan struct{} {
	t.Helper()
	return startMounted(t, exec.Command(sandboxBin, e.args(extra...)...), e.target)
}

// daemon starts the sandbox and returns the daemon's host pid.
func (e *env) daemon(t *testing.T, extra ...string) int {
	t.Helper()
	cmd := exec.Command(sandboxBin, e.args(extra...)...)
	startMounted(t, cmd, e.target)
	ents, err := os.ReadDir("/proc")
	must(t, err)
	for _, d := range ents {
		b, err := os.ReadFile(filepath.Join("/proc", d.Name(), "stat"))
		if err != nil {
			continue
		}
		if f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:])); f[1] == strconv.Itoa(cmd.Process.Pid) {
			pid, _ := strconv.Atoi(d.Name())
			return pid
		}
	}
	t.Fatal("no daemon among the sandbox's children")
	return 0
}

func startMounted(t *testing.T, cmd *exec.Cmd, target string) <-chan struct{} {
	t.Helper()
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	must(t, cmd.Start())
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = unix.Unmount(target, unix.MNT_DETACH)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for !isFUSE(target) {
		select {
		case <-exited:
			t.Fatalf("sandbox exited before mounting: %v\n%s", cmd.ProcessState, &out)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no FUSE mount at %s within 10s\n%s", target, &out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return exited
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func isFUSE(path string) bool {
	var st unix.Statfs_t
	return unix.Statfs(path, &st) == nil && st.Type == fuseSuperMagic
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	must(t, err)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func mountCount(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/mountinfo")
	must(t, err)
	return strings.Count(string(b), "\n")
}

func wantContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != want {
		t.Errorf("read %s = %q, %v; want %q", path, got, err, want)
	}
}

func TestSandboxHoldsOnlyWhatIsBound(t *testing.T) {
	e := newEnv(t)
	before := mountCount(t)
	e.start(t, "-bind", e.data+":/data")

	if got := mountCount(t) - before; got != 1 {
		t.Errorf("host gained %d mounts, want only the target", got)
	}
	binDir := strings.Split(strings.TrimPrefix(loopfsBin, "/"), "/")[0]
	want := []string{binDir, "data", "dev", "proc", "t"}
	slices.Sort(want)
	if got := names(t, e.target); !slices.Equal(got, want) {
		t.Errorf("sandbox root = %v, want %v", got, want)
	}
	if got, want := names(t, filepath.Join(e.target, "dev")), []string{"fuse", "null"}; !slices.Equal(got, want) {
		t.Errorf("sandbox /dev = %v, want %v", got, want)
	}
	wantContent(t, filepath.Join(e.target, "data", "file"), "data")

	if got := names(t, filepath.Join(e.target, "proc")); !slices.Equal(got, []string{"self"}) {
		t.Errorf("sandbox /proc = %v, want only self", got)
	}
	if got := names(t, filepath.Join(e.target, "proc", "self")); !slices.Equal(got, []string{"fd"}) {
		t.Errorf("sandbox /proc/self = %v, want only fd", got)
	}
}

func TestBindsAreNoexec(t *testing.T) {
	e := newEnv(t)
	pid := e.daemon(t, "-bind", e.data+":/data")
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/mountinfo", pid))
	must(t, err)
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) > 5 && f[4] == "/data" {
			if !slices.Contains(strings.Split(f[5], ","), "noexec") {
				t.Errorf("/data mounted %s, want noexec", f[5])
			}
			return
		}
	}
	t.Error("no /data in the daemon's mountinfo")
}

func TestNamespaces(t *testing.T) {
	for _, share := range []bool{false, true} {
		var extra []string
		if share {
			extra = append(extra, "-share-net")
		}
		pid := newEnv(t).daemon(t, extra...)
		for _, ns := range []string{"mnt", "pid", "ipc", "uts", "cgroup", "net"} {
			host, err := os.Readlink("/proc/self/ns/" + ns)
			must(t, err)
			daemon, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", pid, ns))
			must(t, err)
			if want := !share || ns != "net"; (host != daemon) != want {
				t.Errorf("share-net=%v: daemon has its own %s namespace: %v, want %v", share, ns, host != daemon, want)
			}
		}
	}
}

func TestUnmountingTheTargetStopsTheSandbox(t *testing.T) {
	e := newEnv(t)
	exited := e.start(t)
	must(t, unix.Unmount(e.target, unix.MNT_DETACH))
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("sandbox still running 5s after its target was unmounted")
	}
}

func TestBindStaysOnTheCheckedDirectory(t *testing.T) {
	e := newEnv(t)
	e.start(t, "-bind", e.data+":/data")

	must(t, os.Rename(e.data, e.data+".old"))
	must(t, os.Symlink(e.secret, e.data))

	if got := names(t, filepath.Join(e.target, "data")); slices.Contains(got, "shadow") {
		t.Errorf("swapped-in directory reached the sandbox: %v", got)
	}
	wantContent(t, filepath.Join(e.target, "data", "file"), "data")
}

func TestHostSubmountsPropagateIn(t *testing.T) {
	e := newEnv(t)
	e.start(t, "-bind", e.data+":/data")

	sub := filepath.Join(e.data, "sub")
	must(t, unix.Mount("late", sub, "tmpfs", 0, ""))
	t.Cleanup(func() { _ = unix.Unmount(sub, unix.MNT_DETACH) })
	must(t, os.WriteFile(filepath.Join(sub, "late"), []byte("late"), 0o644))

	wantContent(t, filepath.Join(e.target, "data", "sub", "late"), "late")
}

func TestReadOnlyBind(t *testing.T) {
	e := newEnv(t)
	e.start(t, "-ro-bind", e.data+":/data")

	wantContent(t, filepath.Join(e.target, "data", "file"), "data")
	err := os.WriteFile(filepath.Join(e.target, "data", "new"), nil, 0o644)
	if !errors.Is(err, unix.EROFS) {
		t.Errorf("write through a read-only bind = %v, want EROFS", err)
	}
}

func TestRefusesSymlinkedHostPaths(t *testing.T) {
	e := newEnv(t)
	link := filepath.Join(e.root, "link")
	must(t, os.Symlink(e.data, link))

	out, err := exec.Command(sandboxBin, e.args("-bind", link+":/data")...).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "contains a symlink") {
		t.Errorf("bind of a symlinked path = %v, %s; want a refusal", err, out)
	}
	if isFUSE(e.target) {
		t.Error("target was mounted despite the refusal")
	}
}

func TestCommand(t *testing.T) {
	e := newEnv(t)
	cmd, err := Command(&Config{
		Target:  Bind{Host: e.target, Sandbox: "/t"},
		Binds:   []Bind{{Host: e.data, Sandbox: "/data", ReadOnly: true}},
		Devices: []string{"/dev/fuse"},
		Command: []string{loopfsBin, "/", "/t"},
	})
	must(t, err)
	exited := startMounted(t, cmd, e.target)

	wantContent(t, filepath.Join(e.target, "data", "file"), "data")
	must(t, unix.Unmount(e.target, unix.MNT_DETACH))
	<-exited
	if !cmd.ProcessState.Success() {
		t.Errorf("sandbox exited with %v after its target was unmounted", cmd.ProcessState)
	}
}
