package sandbox

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	valid := func() Config {
		return Config{
			Target:  Bind{Host: "/k", Sandbox: "/t"},
			Binds:   []Bind{{Host: "/a", Sandbox: "/a"}},
			Devices: []string{"/dev/fuse"},
			Command: []string{"/bin/d"},
		}
	}
	if cfg := valid(); cfg.Validate() != nil {
		t.Fatalf("valid config rejected: %v", cfg.Validate())
	}

	for name, mutate := range map[string]func(*Config){
		"no target":              func(c *Config) { c.Target = Bind{} },
		"no command":             func(c *Config) { c.Command = nil },
		"relative sandbox path":  func(c *Config) { c.Target.Sandbox = "t" },
		"unclean host path":      func(c *Config) { c.Target.Host = "/k/../x" },
		"relative command":       func(c *Config) { c.Command = []string{"d"} },
		"bind over root":         func(c *Config) { c.Binds[0].Sandbox = "/" },
		"bind inside target":     func(c *Config) { c.Binds[0].Sandbox = "/t/a" },
		"bind over proc":         func(c *Config) { c.Binds[0].Sandbox = "/proc" },
		"bind over command":      func(c *Config) { c.Binds[0].Sandbox = "/bin" },
		"bind over devices":      func(c *Config) { c.Binds[0].Sandbox = "/dev" },
		"device outside of /dev": func(c *Config) { c.Devices = []string{"/tmp/x"} },
		"NUL in sandbox path":    func(c *Config) { c.Binds[0].Sandbox = "/a\x00" },
		"NUL in daemon argument": func(c *Config) { c.Command = append(c.Command, "\x00") },
		"sandbox name too long":  func(c *Config) { c.Binds[0].Sandbox = "/" + strings.Repeat("a", 256) },
		"sandbox path too long":  func(c *Config) { c.Binds[0].Sandbox = strings.Repeat("/a", 2048) },
	} {
		cfg := valid()
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSpecKeepsNonUTF8(t *testing.T) {
	cfg := &Config{
		Target:  Bind{Host: "/k\xe5", Sandbox: "/t\xff"},
		Binds:   []Bind{{Host: "/a\xc3", Sandbox: "/a", ReadOnly: true}},
		Devices: []string{"/dev/fuse"},
		Command: []string{"/bin/d", "-o", "x=\xfe"},
	}
	spec, err := encodeSpec(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSpec(spec)
	if err != nil || !reflect.DeepEqual(got, cfg) {
		t.Errorf("decodeSpec = %+v, %v; want %+v", got, err, cfg)
	}
}
