// Command mountable mounts Mountable filesystems.
//
//	mountable login                     sign in this machine (device flow)
//	mountable mount [--ro] FS_ID DIR    mount with your login
//	mountable mount --ticket-stdin DIR  mount with a ticket from a backend
//	mountable unmount DIR
//	mountable logout
//	mountable version
package main

import (
	"fmt"
	"os"
)

const usage = `usage:
  mountable login
  mountable mount [--ro] FILESYSTEM_ID DIR
  mountable mount --ticket-stdin DIR
  mountable unmount DIR
  mountable logout
  mountable version

Environment:
  MOUNTABLE_API_URL  the Mountable API (default %s)
  MOUNTABLE_WEED     path to weed (default: SeaweedFS %s, downloaded on first mount)
`

// version is set at release build time: -ldflags "-X main.version=X.Y.Z".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, defaultAPI, weedVersion)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "login":
		err = login()
	case "logout":
		err = logout()
	case "mount":
		err = mountCommand(os.Args[2:])
	case "version":
		fmt.Printf("mountable %s (SeaweedFS %s)\n", version, weedVersion)
	case "unmount":
		if len(os.Args) != 3 {
			err = fmt.Errorf("usage: mountable unmount DIR")
		} else {
			err = unmount(os.Args[2])
		}
	default:
		fmt.Fprintf(os.Stderr, usage, defaultAPI, weedVersion)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mountable:", err)
		os.Exit(1)
	}
}
