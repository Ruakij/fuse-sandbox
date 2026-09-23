// Package sandbox runs a FUSE daemon in an empty root that holds only the paths
// bound into it, while its mount on the sandbox target propagates to the host.
//
// The sandbox is built by a re-executed copy of the calling program, so a
// program using [Command] must call [Init] first thing in main.
package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// Command is the daemon and its arguments. The binary is bound at its own
	// path with nothing else, so it must be static.
	Command []string
}

// initArg marks the re-executed child that builds the sandbox.
const initArg = "__fuse-sandbox-init"

// Always present: libfuse opens /dev/null on startup.
var defaultDevices = []string{"/dev/null"}

// Init builds the sandbox and execs the daemon if this process was started by
// [Command], and returns otherwise. Call it before anything else in main.
func Init() {
	if len(os.Args) != 3 || os.Args[1] != initArg {
		return
	}
	var cfg Config
	err := json.Unmarshal([]byte(os.Args[2]), &cfg)
	if err == nil {
		err = enter(&cfg)
	}
	fmt.Fprintf(os.Stderr, "fuse-sandbox: %v\n", err)
	os.Exit(1)
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
	dsts := []string{c.Target.Sandbox, c.Command[0], "/proc"}
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

	for _, p := range append(hosts, dsts...) {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("path %q must be absolute and clean", p)
		}
	}
	for i, a := range dsts {
		if a == "/" {
			return errors.New("nothing may be bound over the sandbox root")
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
