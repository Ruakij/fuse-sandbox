# fuse-sandbox

[![Linux](https://img.shields.io/badge/Linux-5.12%2B-FCC624?logo=linux&logoColor=black)](#requirements)
[![Go Reference](https://pkg.go.dev/badge/github.com/Ruakij/fuse-sandbox/pkg/sandbox.svg)](https://pkg.go.dev/github.com/Ruakij/fuse-sandbox/pkg/sandbox)

Runs a FUSE daemon in an empty root that holds only the paths bound into it.
The daemon's mount still shows up on the host; nothing else it could reach
exists outside the sandbox.

```sh
fuse-sandbox \
  -target /var/lib/app/mnt:/mnt \
  -ro-bind /srv/a:/a -bind /srv/b:/b \
  -dev /dev/fuse \
  -- /usr/local/bin/mergerfs -f -o allow_other /a:/b /mnt
```

The daemon sees `/a`, `/b`, `/mnt`, `/dev/fuse`, `/dev/null`, its own binary and
`/proc/self/fd`. Its mount on `/mnt` appears at `/var/lib/app/mnt`.

## Why

A FUSE daemon serving files for someone else can often be steered by what it
serves: a symlink it follows, a runtime control interface that adds a branch, a
path swapped under it. Each daemon has its own knobs to turn that off, and
missing one means the consumer reads host files. In the sandbox none of it
matters, because there is nothing else to reach.

Existing sandboxes (bubblewrap, nsjail, firejail, systemd's `RootDirectory=`)
build a private mount namespace too, but they cut mount propagation to the host,
so the daemon's mount never leaves the sandbox. fuse-sandbox keeps exactly one
path propagating: the target.

Where a container runtime is at hand, a container does the same: Docker runs a
FUSE daemon with `--cap-add SYS_ADMIN --device /dev/fuse` and a `:rshared`
volume, and its image brings `fusermount3`, CA certificates and the like. In
Kubernetes, though, a mount only propagates out of a pod with
`mountPropagation: Bidirectional`, which requires a privileged container: every
host device, every capability, no seccomp or AppArmor. fuse-sandbox runs inside
such a pod and gives the daemon back an empty root. It also needs no runtime,
for a mount tied to one process, such as a systemd unit.

## Requirements

- Linux 5.12 or newer (`mount_setattr`), run as root.
- The host side of the target must be on a shared mount, e.g. `mount
  --make-rshared`, as kubelet's pods directory is.
- The daemon binary is bound at its path, alone: it must be static, or have its
  loader and libraries bound too.
- All paths absolute, clean and without symlinks. Resolve them first; a symlink
  anywhere in a host path is refused.
- libfuse daemons need `/dev/fuse` passed with `-dev`. `/dev/null` is always
  there.
- Daemons that mount through `fusermount3`, like rclone (bazil.org/fuse) and
  go-fuse without `DirectMount`, need it bound where they look, with `PATH` set
  for those that search it. Running as root, it needs no setuid bit and no
  `/etc/fuse.conf`.

## Limits

This contains a daemon that is tricked into reaching other paths. It is not a
boundary against code execution in the daemon: the daemon keeps root and
`CAP_SYS_ADMIN`, which it needs to mount FUSE, and a root process with it has
ways out of a mount namespace.

Of the files fuse-sandbox inherits, only stdio reaches the daemon, and it stays
reachable through `/proc/self/fd`, so stdio should be pipes or `/dev/null`, not
host files.

## Install

- Binaries for amd64, arm64, arm, 386, riscv64, ppc64le and s390x, with
  `SHA256SUMS` and build provenance, are attached to each
  [release](https://github.com/Ruakij/fuse-sandbox/releases).
- A `scratch` image to copy from:

  ```dockerfile
  COPY --from=ghcr.io/ruakij/fuse-sandbox:vX.Y.Z /fuse-sandbox /usr/local/bin/fuse-sandbox
  ```

- From source: `go install github.com/Ruakij/fuse-sandbox/cmd/fuse-sandbox@latest`

## Usage

```
fuse-sandbox -target HOST:SANDBOX [flags] -- DAEMON [ARGS...]

  -target HOST:SANDBOX   where the daemon mounts; SANDBOX shows up at HOST
  -bind HOST:SANDBOX     path to bind read-write, with its submounts (repeatable)
  -ro-bind HOST:SANDBOX  path to bind read-only, with its submounts (repeatable)
  -dev DEVICE            character device to bind at the same path (repeatable)
  -share-net             keep the host's network, for daemons serving remote files
  -mountinfo             add /proc/self/mountinfo, for daemons that check their mount
  -version               print the version and exit
```

`-mountinfo` shows the daemon the host paths of its binds, their devices and
mount options, but no file contents. rclone needs it to unmount on exit, and
`--allow-non-empty`, as it sees the target as a mount already:

```sh
env -i PATH=/usr/bin fuse-sandbox -target /mnt/remote:/mnt -share-net -mountinfo \
  -ro-bind /opt/fusermount3:/usr/bin/fusermount3 -dev /dev/fuse \
  -ro-bind /etc/ssl/certs:/etc/ssl/certs -ro-bind /etc/resolv.conf:/etc/resolv.conf \
  -bind /var/lib/rclone:/rclone \
  -- /usr/local/bin/rclone mount remote: /mnt --allow-non-empty \
     --config /rclone/rclone.conf --cache-dir /rclone/cache
```

The daemon inherits the environment; start fuse-sandbox under `env -i NAME=VALUE
...` to pass it only what it needs.

It forwards `SIGTERM`, `SIGINT` and `SIGHUP` to the daemon, takes the daemon down
with it if killed, and exits with the daemon's exit code, or 128 plus the signal
that ended it.

## As a library

```go
import "github.com/Ruakij/fuse-sandbox/pkg/sandbox"

func main() {
	sandbox.Init() // builds the sandbox when re-executed by Command, else returns

	cmd, err := sandbox.Command(&sandbox.Config{
		Target:  sandbox.Bind{Host: "/var/lib/app/mnt", Sandbox: "/mnt"},
		Binds:   []sandbox.Bind{{Host: "/srv/a", Sandbox: "/a", ReadOnly: true}},
		Devices: []string{"/dev/fuse"},
		Command: []string{"/usr/local/bin/mergerfs", "-f", "/a", "/mnt"},
	})
	// cmd is an *exec.Cmd: set its I/O, start it, wait for it.
}
```

`Command` leaves the daemon's lifetime to the caller: it survives the caller
unless `cmd.SysProcAttr.Pdeathsig` is set. `cmd.ExtraFiles` do not reach the
daemon; `cmd.Env` becomes its environment. `Run` is the foreground behaviour of the command line tool.

## How it works

1. The process re-executes itself as PID 1 of new mount, PID, IPC, UTS and
   cgroup namespaces, and of an empty network namespace unless `-share-net` is
   given.
2. It clones the target mount while that is still a peer of the host's shared
   mount, then makes every other mount a slave, so nothing mounted inside
   propagates out.
3. Each host path is opened with `openat2(RESOLVE_NO_SYMLINKS)` and the mount is
   cloned from that file descriptor, so what gets bound is the inode that was
   checked, not whatever the path points at a moment later. Binds are
   `nosuid,nodev` and recursive, and later host submounts still propagate in.
4. An empty tmpfs becomes the new root with the clones attached, and is made
   read-only after `pivot_root`. Of `proc` it holds only the daemon's own
   `/proc/self/fd`, read-only, which daemons like mergerfs need to reopen their
   files, and with `-mountinfo` its `/proc/self/mountinfo`. The rest would let a
   daemon that follows a symlink into it read its own memory and environment.
5. It sets `no_new_privs`, cuts the capability bounding set to what a FUSE
   daemon serving files as root uses (`CAP_SYS_ADMIN` to mount, the file
   capabilities to act for callers of any uid), closes every inherited file
   descriptor but stdio and execs the daemon, which becomes PID 1. Unmounting
   the target on the host ends the daemon, and with it the sandbox.

## Development

```sh
git config core.hooksPath .githooks  # gofmt, vet, tests and lint before each commit
make build       # bin/fuse-sandbox
make test-mount  # real sandboxes in a privileged container, needs Docker
make fuzz        # command lines, FUZZTIME=30s by default
make fuzz-mount  # bind layouts through real sandboxes, like test-mount
make dist        # release binaries and SHA256SUMS
```

The mount tests run a small go-fuse loopback daemon (`test/loopfs`) that serves
the sandbox's own root at the target, so the host sees exactly what the daemon
can reach.

Tagging `vX.Y.Z` publishes a release, `vX.Y.Z-rc.N` a prerelease; both attach
the binaries and push the image.
