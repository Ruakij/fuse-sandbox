// Command fuse-sandbox runs a FUSE daemon in an empty root that holds only the
// paths bound into it. The daemon's mount on the sandbox target shows up at the
// host target; nothing else it can reach exists outside the sandbox.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Ruakij/fuse-sandbox/pkg/sandbox"
)

var version = "dev"

func main() {
	sandbox.Init()

	args := os.Args[1:]
	if len(args) == 1 && (args[0] == "-version" || args[0] == "--version") {
		fmt.Println(version)
		return
	}
	cfg, err := parse(args)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuse-sandbox: %v\n", err)
		os.Exit(2)
	}
	code, err := sandbox.Run(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuse-sandbox: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}
