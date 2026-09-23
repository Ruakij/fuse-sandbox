package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParse drives command lines, NUL-separated as argv holds no NUL, through
// parsing and validation. Whatever is accepted must be a sandbox whose paths are
// absolute and clean and cannot shadow each other.
func FuzzParse(f *testing.F) {
	f.Add("-target\x00/k/p:/t\x00-bind\x00/a:b:/a\x00-ro-bind\x00/c:/c\x00-dev\x00/dev/fuse\x00-share-net\x00--\x00/bin/d\x00-f")
	f.Add("-target\x00/k:/t\x00-bind\x00/t/a:/t/a\x00--\x00/bin/d")
	f.Add("-target\x00/k:/dev\x00-dev\x00/dev/../x\x00--\x00d")

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, cmdline string) {
		// flag prints every rejected command line with its usage. Not in the setup
		// above, which the coordinator runs too, reporting its progress on stderr.
		stderr := os.Stderr
		os.Stderr = devNull
		cfg, err := parse(strings.Split(cmdline, "\x00"))
		os.Stderr = stderr
		if err != nil {
			return
		}

		hosts := []string{cfg.Target.Host, cfg.Command[0]}
		dsts := []string{cfg.Target.Sandbox, cfg.Command[0], "/proc", "/dev/null"}
		for _, b := range cfg.Binds {
			hosts = append(hosts, b.Host)
			dsts = append(dsts, b.Sandbox)
		}
		for _, d := range cfg.Devices {
			if !strings.HasPrefix(d, "/dev/") {
				t.Fatalf("accepted device %q outside of /dev", d)
			}
			dsts = append(dsts, d)
		}
		for _, p := range append(hosts, dsts...) {
			if !filepath.IsAbs(p) || filepath.Clean(p) != p {
				t.Fatalf("accepted path %q", p)
			}
		}
		for i, a := range dsts {
			for _, b := range dsts[i+1:] {
				if a == "/" || b == "/" || strings.HasPrefix(b+"/", a+"/") || strings.HasPrefix(a+"/", b+"/") {
					t.Fatalf("accepted overlapping sandbox paths %q and %q", a, b)
				}
			}
		}
	})
}
