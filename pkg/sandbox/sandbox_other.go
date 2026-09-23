//go:build !linux

package sandbox

import (
	"errors"
	"os/exec"
)

var errUnsupported = errors.New("fuse-sandbox needs Linux")

func Command(*Config) (*exec.Cmd, error) { return nil, errUnsupported }

func Run(*Config) (int, error) { return 0, errUnsupported }

func enter(*Config) error { return errUnsupported }
