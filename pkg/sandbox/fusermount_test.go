package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

func TestFusermountArgs(t *testing.T) {
	for _, c := range []struct {
		args           []string
		op             byte
		opts, mnt, err string
	}{
		{args: []string{"-o", "rw,allow_other", "--", "/t"}, op: opMount, opts: "rw,allow_other", mnt: "/t"},
		{args: []string{"/t", "-o", "fsname=x"}, op: opMount, opts: "fsname=x", mnt: "/t"},
		{args: []string{"-oro", "-o", "noexec", "/t"}, op: opMount, opts: "ro,noexec", mnt: "/t"},
		{args: []string{"-u", "/t"}, op: opUnmount, mnt: "/t"},
		{args: []string{"-u", "-q", "-z", "--", "-t"}, op: opUnmountLazy, mnt: "-t"},
		{args: []string{"-z", "/t"}, err: "-z needs -u"},
		{args: []string{"--auto-unmount", "/t"}, err: "not supported"},
		{args: []string{"-o"}, err: "needs options"},
		{args: []string{"/a", "/b"}, err: "one mountpoint"},
		{args: nil, err: "one mountpoint"},
	} {
		op, opts, mnt, err := fusermountArgs(c.args)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: err = %v, want %q", c.args, err, c.err)
			}
			continue
		}
		if err != nil || op != c.op || opts != c.opts || mnt != c.mnt {
			t.Errorf("%q = %c, %q, %q, %v; want %c, %q, %q", c.args, op, opts, mnt, err, c.op, c.opts, c.mnt)
		}
	}
}

func TestParseMountOptions(t *testing.T) {
	got, err := parseMountOptions("rw,nosuid,nodev,allow_other,fsname=a b,subtype=loop,max_read=0131072,ro,noexec,noatime")
	want := fuseMount{
		source: "a b", subtype: "loop", readOnly: true, noexec: true, noatime: true,
		opts: []string{"allow_other", "max_read=131072"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("parseMountOptions = %+v, %v; want %+v", got, err, want)
	}

	for _, o := range []string{
		"suid", "dev", "auto_unmount", "fd=3", "rootmode=40000", "user_id=1", "group_id=1",
		"max_read=x", "max_read=4294967296", "fsname=", "fsname=a\\,fd=3", "subtype=a\x00", "blksize=4096",
	} {
		if _, err := parseMountOptions(o); err == nil {
			t.Errorf("%q: accepted", o)
		}
	}
}

// FuzzParseMountOptions checks that nothing the daemon asks for adds a mount
// option the supervisor did not allow, as the data it builds is comma-separated.
func FuzzParseMountOptions(f *testing.F) {
	f.Add("rw,allow_other,fsname=x,subtype=y,max_read=4096")
	f.Add("fd=3")
	f.Fuzz(func(t *testing.T, s string) {
		m, err := parseMountOptions(s)
		if err != nil {
			return
		}
		for _, v := range append([]string{m.source, m.subtype}, m.opts...) {
			if strings.ContainsAny(v, ",\x00\\") {
				t.Errorf("%q passes on %q", s, v)
			}
		}
		for _, o := range m.opts {
			if k, _, _ := strings.Cut(o, "="); k != "allow_other" && k != "default_permissions" && k != "max_read" {
				t.Errorf("%q passes on %q", s, o)
			}
		}
	})
}
