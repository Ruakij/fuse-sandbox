// Command loopfs is a test daemon: it mirrors a directory over FUSE. The mount tests run it inside the
// sandbox on the sandbox's own root, so the host sees exactly what a daemon can.
package main

import (
	"log"
	"os"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: loopfs SRC MOUNTPOINT")
	}
	root, err := fs.NewLoopbackRoot(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	// Strict direct mount: the sandbox has no fusermount to fall back to.
	server, err := fs.Mount(os.Args[2], root, &fs.Options{MountOptions: fuse.MountOptions{
		AllowOther:         true,
		DirectMountStrict:  true,
		FsName:             "loopfs",
		DisableReadDirPlus: true,
	}})
	if err != nil {
		log.Fatal(err)
	}
	server.Wait()
}
