package sandbox

import "testing"

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
	} {
		cfg := valid()
		mutate(&cfg)
		if cfg.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
