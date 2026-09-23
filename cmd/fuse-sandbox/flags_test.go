package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Ruakij/fuse-sandbox/pkg/sandbox"
)

func TestParse(t *testing.T) {
	cfg, err := parse(strings.Fields("-target /k/p:/t -bind /a:b:/a -ro-bind /c:/c -dev /dev/fuse -share-net -- /bin/d -f /t"))
	if err != nil {
		t.Fatal(err)
	}
	want := &sandbox.Config{
		Target:   sandbox.Bind{Host: "/k/p", Sandbox: "/t"},
		Binds:    []sandbox.Bind{{Host: "/a:b", Sandbox: "/a"}, {Host: "/c", Sandbox: "/c", ReadOnly: true}},
		Devices:  []string{"/dev/fuse"},
		ShareNet: true,
		Command:  []string{"/bin/d", "-f", "/t"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("parse = %+v, want %+v", cfg, want)
	}
}

func TestParseRejectsBindsWithoutSandboxPath(t *testing.T) {
	if _, err := parse(strings.Fields("-target /k:/t -bind /a -- /bin/d")); err == nil {
		t.Error("parse accepted -bind without a sandbox path")
	}
}
