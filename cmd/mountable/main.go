// Command mountable mounts Mountable filesystems.
//
//	mountable login                     sign in this machine (device flow)
//	mountable mount [--ro] FS_ID DIR    mount with your login
//	mountable mount --ticket-stdin DIR  mount with a ticket from a backend
//	mountable unmount DIR
//	mountable logout
//	mountable version
//	mountable licenses
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/writeitai/mountable"
)

const usage = `usage:
  mountable login
  mountable mount [--ro] FILESYSTEM_ID DIR
  mountable mount --ticket-stdin DIR
  mountable unmount DIR
  mountable logout
  mountable version
  mountable licenses   third-party licenses and notices

Environment:
  MOUNTABLE_API_URL  the Mountable API (default %s)
`

// version is set at release build time: -ldflags "-X main.version=X.Y.Z".
var version = "dev"

func main() {
	takeOverSignals()
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, defaultAPI)
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
		printVersion(os.Stdout)
	case "licenses":
		printLicenses(os.Stdout)
	case "unmount":
		if len(os.Args) != 3 {
			err = fmt.Errorf("usage: mountable unmount DIR")
		} else {
			err = unmount(os.Args[2])
		}
	default:
		fmt.Fprintf(os.Stderr, usage, defaultAPI)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mountable:", err)
		os.Exit(1)
	}
}

// takeOverSignals removes the handler the mount engine's packages install at
// init, which exits the process on SIGINT, SIGTERM, SIGHUP or SIGALRM. This
// process decides how a mount ends (see lifecycle.go); other signals keep
// their default behaviour.
func takeOverSignals() {
	signal.Reset(os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGALRM)
}

func printVersion(out io.Writer) {
	fmt.Fprintf(out, "mountable %s\n", version)
}

func printLicenses(out io.Writer) {
	fmt.Fprint(out, mountable.ThirdPartyNotices)
}
