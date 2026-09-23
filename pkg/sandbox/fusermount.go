package sandbox

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Requests from the fusermount stand-in to the supervisor: the op, then the
// mount options, with an fd on the mountpoint attached.
const (
	opMount       = 'm'
	opUnmount     = 'u'
	opUnmountLazy = 'z'
)

// fusermountArgs parses a fusermount command line as libfuse, go-fuse and
// bazil.org/fuse pass it.
func fusermountArgs(args []string) (op byte, opts, mountpoint string, err error) {
	var unmount, lazy bool
	var o, mnts []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			mnts = append(mnts, args[i+1:]...)
			i = len(args)
		case a == "-o":
			if i++; i == len(args) {
				return 0, "", "", errors.New("-o needs options")
			}
			o = append(o, args[i])
		case strings.HasPrefix(a, "-o"):
			o = append(o, a[2:])
		case a == "-u":
			unmount = true
		case a == "-z":
			lazy = true
		case a == "-q":
		case strings.HasPrefix(a, "-"):
			return 0, "", "", fmt.Errorf("%s is not supported in the sandbox", a)
		default:
			mnts = append(mnts, a)
		}
	}
	if len(mnts) != 1 {
		return 0, "", "", errors.New("want exactly one mountpoint")
	}
	switch {
	case lazy && !unmount:
		return 0, "", "", errors.New("-z needs -u")
	case lazy:
		op = opUnmountLazy
	case unmount:
		op = opUnmount
	default:
		op = opMount
	}
	return op, strings.Join(o, ","), mnts[0], nil
}

// fuseMount is a FUSE mount the supervisor makes on the daemon's behalf.
type fuseMount struct {
	source, subtype           string
	readOnly, noexec, noatime bool
	// opts go into the mount data, after what the supervisor sets itself.
	opts []string
}

// parseMountOptions allows what a FUSE daemon needs and nothing that reaches
// past its own mount. nosuid and nodev are always set.
func parseMountOptions(s string) (fuseMount, error) {
	var m fuseMount
	for _, o := range strings.Split(s, ",") {
		switch o {
		case "", "nosuid", "nodev", "async":
		case "rw", "ro":
			m.readOnly = o == "ro"
		case "exec", "noexec":
			m.noexec = o == "noexec"
		case "atime", "noatime":
			m.noatime = o == "noatime"
		case "allow_other", "default_permissions":
			m.opts = append(m.opts, o)
		default:
			k, v, _ := strings.Cut(o, "=")
			if k == "max_read" {
				n, err := strconv.ParseUint(v, 10, 32)
				if err != nil {
					return fuseMount{}, fmt.Errorf("max_read: %w", err)
				}
				m.opts = append(m.opts, "max_read="+strconv.FormatUint(n, 10))
				continue
			}
			if (k != "fsname" && k != "subtype") || v == "" || strings.ContainsAny(v, "\\\x00") {
				return fuseMount{}, fmt.Errorf("mount option %q is not allowed in the sandbox", o)
			}
			if k == "fsname" {
				m.source = v
			} else {
				m.subtype = v
			}
		}
	}
	return m, nil
}
