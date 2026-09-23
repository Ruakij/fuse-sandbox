package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/Ruakij/fuse-sandbox/pkg/sandbox"
)

// parse parses and validates a fuse-sandbox command line, without the program
// name.
func parse(args []string) (*sandbox.Config, error) {
	var cfg sandbox.Config
	fs := flag.NewFlagSet("fuse-sandbox", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: fuse-sandbox -target HOST:SANDBOX [flags] -- DAEMON [ARGS...]\n\n")
		fs.PrintDefaults()
	}
	fs.Func("target", "`HOST:SANDBOX` mountpoint; the daemon's mount on SANDBOX appears at HOST, which must be on a shared mount", func(v string) error {
		b, err := parseBind(v)
		cfg.Target = b
		return err
	})
	fs.Func("bind", "`HOST:SANDBOX` path to bind read-write, with its submounts (repeatable)", func(v string) error {
		b, err := parseBind(v)
		cfg.Binds = append(cfg.Binds, b)
		return err
	})
	fs.Func("ro-bind", "`HOST:SANDBOX` path to bind read-only, with its submounts (repeatable)", func(v string) error {
		b, err := parseBind(v)
		b.ReadOnly = true
		cfg.Binds = append(cfg.Binds, b)
		return err
	})
	fs.Func("dev", "character `device` to bind at the same path, e.g. /dev/fuse (repeatable)", func(v string) error {
		cfg.Devices = append(cfg.Devices, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg.Command = fs.Args()
	return &cfg, cfg.Validate()
}

// parseBind splits at the last colon: sandbox paths are chosen by the caller
// and never need one, host paths might.
func parseBind(v string) (sandbox.Bind, error) {
	i := strings.LastIndex(v, ":")
	if i <= 0 {
		return sandbox.Bind{}, fmt.Errorf("%q: want HOST:SANDBOX", v)
	}
	return sandbox.Bind{Host: v[:i], Sandbox: v[i+1:]}, nil
}
