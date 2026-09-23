// Package sandbox runs a FUSE daemon in an empty root that holds only the paths
// bound into it, while its mount on the sandbox target propagates to the host.
//
// The sandbox is built by a re-executed copy of the calling program, which also
// runs inside it, so a program using [Command] must be static and call [Init]
// first thing in main.
package sandbox

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Bind maps a host path to a path inside the sandbox.
type Bind struct {
	Host, Sandbox string
	ReadOnly      bool
}

// Config describes a sandbox. All paths must be absolute, clean and free of
// symlinks.
type Config struct {
	// Target is where the daemon mounts. The host side must be on a shared
	// mount, or the daemon's mount does not propagate out.
	Target Bind
	// Binds are the only host paths the daemon can reach, with their submounts.
	Binds []Bind
	// Devices are character devices bound at their own path, e.g. /dev/fuse.
	Devices []string
	// ShareNet keeps the host's network namespace, for daemons serving remote
	// files such as sshfs or rclone. Otherwise the daemon's is empty.
	ShareNet bool
	// Command is the daemon and its arguments. The binary is bound at its own
	// path with nothing else, so it must be static.
	Command []string
	// Env is the daemon's environment; nil passes on the caller's. It reaches the
	// sandbox as its environment, not in its argv, which any host user can read.
	Env []string
	// Fusermount are the sandbox paths of the fusermount stand-in, through which
	// the daemon mounts the target. Each name must contain "fusermount". Nil
	// means /usr/bin/fusermount3 and /bin/fusermount3, where libfuse, go-fuse
	// and bazil.org/fuse look.
	Fusermount []string
}

func (c *Config) fusermount() []string {
	if len(c.Fusermount) == 0 {
		return []string{"/usr/bin/fusermount3", "/bin/fusermount3"}
	}
	return c.Fusermount
}

const (
	// initArg marks the re-executed child that builds the sandbox and supervises
	// the daemon.
	initArg = "__fuse-sandbox-init"
	// daemonArg marks the supervisor's child, which becomes the daemon.
	daemonArg = "__fuse-sandbox-daemon"
	// sockPath is where the supervisor takes fusermount requests.
	sockPath = "/run/fuse-sandbox.sock"
)

// Always present: libfuse opens /dev/null on startup.
var defaultDevices = []string{"/dev/null"}

// Init runs the sandbox's part of the program if this process was started by
// [Command] or is the fusermount stand-in, and returns otherwise. Call it before
// anything else in main.
func Init() {
	if strings.Contains(filepath.Base(os.Args[0]), "fusermount") {
		if err := fusermount(os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "fusermount: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) != 3 {
		return
	}
	var run func(*Config) error
	switch os.Args[1] {
	case initArg:
		run = supervise
	case daemonArg:
		run = execDaemon
	default:
		return
	}
	cfg, err := decodeSpec(os.Args[2])
	if err == nil {
		err = run(cfg)
	}
	fmt.Fprintf(os.Stderr, "fuse-sandbox: %v\n", err)
	os.Exit(1)
}

// encodeSpec passes c to the child as gob, which unlike JSON keeps strings that
// are not UTF-8, as Linux paths need not be, in base64, as argv holds no NUL.
func encodeSpec(c *Config) (string, error) {
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(c); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b.Bytes()), nil
}

func decodeSpec(spec string) (*Config, error) {
	b, err := base64.StdEncoding.DecodeString(spec)
	if err != nil {
		return nil, err
	}
	var c Config
	return &c, gob.NewDecoder(bytes.NewReader(b)).Decode(&c)
}

// Validate reports whether c is a sandbox that can be built.
func (c *Config) Validate() error {
	if c.Target.Host == "" {
		return errors.New("-target is required")
	}
	if len(c.Command) == 0 {
		return errors.New("no daemon command given")
	}

	// The daemon binary is bound at its own path, so it must be static.
	hosts := []string{c.Target.Host, c.Command[0]}
	dsts := append([]string{c.Target.Sandbox, c.Command[0], "/proc", sockPath}, c.fusermount()...)
	for _, f := range c.fusermount() {
		if !strings.Contains(filepath.Base(f), "fusermount") {
			return fmt.Errorf("fusermount path %q: name must contain \"fusermount\"", f)
		}
	}
	for _, b := range c.Binds {
		hosts = append(hosts, b.Host)
		dsts = append(dsts, b.Sandbox)
	}
	for _, d := range append(c.Devices, defaultDevices...) {
		if !strings.HasPrefix(d, "/dev/") {
			return fmt.Errorf("device %q is not under /dev", d)
		}
		hosts = append(hosts, d)
		dsts = append(dsts, d)
	}

	for _, s := range slices.Concat(hosts, dsts, c.Command, c.Env) {
		if strings.IndexByte(s, 0) >= 0 {
			return fmt.Errorf("%q contains a NUL byte", s)
		}
	}
	for _, p := range append(hosts, dsts...) {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("path %q must be absolute and clean", p)
		}
	}
	for i, a := range dsts {
		if a == "/" {
			return errors.New("nothing may be bound over the sandbox root")
		}
		// The sandbox root is a tmpfs, so these are its limits whatever the host's.
		if len(a) >= 4096 || slices.ContainsFunc(strings.Split(a, "/"), func(n string) bool { return len(n) > 255 }) {
			return fmt.Errorf("sandbox path %.64q... is too long", a)
		}
		for _, b := range dsts[i+1:] {
			if under(a, b) || under(b, a) {
				return fmt.Errorf("sandbox paths %s and %s overlap", a, b)
			}
		}
	}
	return nil
}

func under(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, dir+"/")
}
