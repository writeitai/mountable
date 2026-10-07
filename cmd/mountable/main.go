// Command mountable mounts and manages Mountable filesystems. Run
// `mountable help` for the commands.
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/writeitai/mountable"
)

const usage = `usage:
  mountable login                          sign in this machine (device flow)
  mountable logout
  mountable orgs list
  mountable fs list [--org ORG_ID]
  mountable fs create NAME [--org ORG_ID]
  mountable fs usage FS_ID [--org ORG_ID]
  mountable ticket create FS_ID [--ro] [--idempotency-key KEY]
  mountable sessions list FS_ID [--org ORG_ID]
  mountable sessions revoke SESSION_ID
  mountable mount [--ro] FS_ID DIR         mount with your login
  mountable mount --ticket-stdin DIR       mount with a ticket read from stdin
  mountable unmount DIR
  mountable mcp                            MCP server over stdio
  mountable version
  mountable licenses                       third-party licenses and notices

Every command takes --json: one JSON document on stdout (JSON lines for
mount; {"usage": …} for help), diagnostics on stderr. The exception is mcp,
whose stdout is the MCP protocol. Nothing prompts.

Exit codes: 0 success, 1 error (API, network, mount or credentials),
2 wrong usage.

Environment:
  MOUNTABLE_API_KEY  an API key, used instead of the login
  MOUNTABLE_ORG      the organisation, when --org is not given
  MOUNTABLE_API_URL  the Mountable API (default %s)
`

// version is set at release build time: -ldflags "-X main.version=X.Y.Z".
var version = "dev"

func main() {
	takeOverSignals()
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run runs one command and returns its exit code.
func run(args []string, stdout, stderr io.Writer) int {
	o := &output{stdout: stdout, stderr: stderr}
	if len(args) == 0 {
		fmt.Fprintf(stderr, usage, defaultAPI)
		return 2
	}
	o.json = slices.Contains(args[1:], "--json") || slices.Contains(args[1:], "-json")
	if err := dispatch(o, args[0], args[1:]); err != nil {
		return o.fail(err)
	}
	return 0
}

func dispatch(o *output, command string, args []string) error {
	sub := func(names ...string) (string, []string) {
		if len(args) > 0 && slices.Contains(names, args[0]) {
			return args[0], args[1:]
		}
		return "", nil
	}
	switch command {
	case "help", "-h", "--help":
		text := fmt.Sprintf(usage, defaultAPI)
		return o.result(map[string]string{"usage": text}, func(w io.Writer) error {
			_, err := fmt.Fprint(w, text)
			return err
		})
	case "login":
		return simple(o, args, "login", func() (any, error) {
			return map[string]bool{"signed_in": true}, login(o.messages())
		})
	case "logout":
		return simple(o, args, "logout", func() (any, error) {
			return map[string]bool{"signed_out": true}, logout()
		})
	case "version":
		return simple(o, args, "version", func() (any, error) {
			if !o.json {
				printVersion(o.stdout)
			}
			return map[string]string{"version": version}, nil
		})
	case "licenses":
		return simple(o, args, "licenses", func() (any, error) {
			if !o.json {
				printLicenses(o.stdout)
			}
			return map[string]string{"notices": mountable.ThirdPartyNotices}, nil
		})
	case "unmount":
		f := newFlags("unmount DIR")
		dir, err := f.parse(args, 1)
		if err != nil {
			return err
		}
		if err := unmount(dir[0]); err != nil {
			return err
		}
		return o.result(map[string]string{"unmounted": dir[0]}, func(io.Writer) error { return nil })
	case "mount":
		return mountCommand(o, args)
	case "mcp":
		// stdout carries the MCP protocol, so --json changes nothing.
		if _, err := newFlags("mcp").parse(args, 0); err != nil {
			return err
		}
		return serveMCP()
	case "orgs":
		if name, rest := sub("list"); name != "" {
			return orgsList(o, rest)
		}
		return usageError("orgs list")
	case "fs":
		switch name, rest := sub("list", "create", "usage"); name {
		case "list":
			return fsList(o, rest)
		case "create":
			return fsCreate(o, rest)
		case "usage":
			return fsUsage(o, rest)
		}
		return usageError("fs list | fs create NAME | fs usage FS_ID")
	case "ticket":
		if name, rest := sub("create"); name != "" {
			return ticketCreate(o, rest)
		}
		return usageError("ticket create FS_ID [--ro] [--idempotency-key KEY]")
	case "sessions":
		switch name, rest := sub("list", "revoke"); name {
		case "list":
			return sessionsList(o, rest)
		case "revoke":
			return sessionsRevoke(o, rest)
		}
		return usageError("sessions list FS_ID | sessions revoke SESSION_ID")
	}
	return usageError("COMMAND; run `mountable help`")
}

// simple runs a command without arguments; with --json it writes fn's
// result.
func simple(o *output, args []string, name string, fn func() (any, error)) error {
	if _, err := newFlags(name).parse(args, 0); err != nil {
		return err
	}
	result, err := fn()
	if err != nil || !o.json {
		return err
	}
	return o.emit(result)
}

// messages is where a command's human messages go: stdout, or stderr when
// stdout carries JSON.
func (o *output) messages() io.Writer {
	if o.json {
		return o.stderr
	}
	return o.stdout
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
