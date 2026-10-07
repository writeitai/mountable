package main

// The management commands: organisations, filesystems, tickets, sessions.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
)

// output is where a command writes: with --json, one JSON document (or JSON
// lines for mount) on stdout; otherwise short text. Diagnostics go to stderr
// either way.
type output struct {
	stdout, stderr io.Writer
	json           bool
}

// result writes v as JSON, or calls text to write it for people.
func (o *output) result(v any, text func(w io.Writer) error) error {
	if o.json {
		return o.emit(v)
	}
	return text(o.stdout)
}

// emit writes v as one line of JSON to stdout.
func (o *output) emit(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(o.stdout, "%s\n", data)
	return err
}

// fail reports err and returns the exit code.
func (o *output) fail(err error) int {
	e := toCLIError(err)
	_, reported := err.(*alreadyReported)
	switch {
	case o.json && !reported:
		_ = o.emit(map[string]any{"error": e})
	case !o.json:
		fmt.Fprintf(o.stderr, "mountable: %s (%s)\n", e.Message, e.Code)
		if e.Hint != "" {
			fmt.Fprintf(o.stderr, "mountable: hint: %s\n", e.Hint)
		}
	}
	return e.exitCode()
}

// alreadyReported is an error the command already put in its JSON output.
type alreadyReported struct{ error }

func (e *alreadyReported) Unwrap() error { return e.error }

// flags parses a command's flags wherever they appear among its arguments.
type flags struct {
	*flag.FlagSet
	usage string
}

func newFlags(usage string) *flags {
	f := flag.NewFlagSet(usage, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.Bool("json", false, "write JSON to stdout")
	return &flags{FlagSet: f, usage: usage}
}

// parse requires exactly n positional arguments (any number when n < 0)
// and returns them.
func (f *flags) parse(args []string, n int) ([]string, error) {
	var positional []string
	for {
		if err := f.Parse(args); err != nil {
			return nil, usageError(f.usage)
		}
		args = f.Args()
		if len(args) == 0 {
			break
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
	if n >= 0 && len(positional) != n {
		return nil, usageError(f.usage)
	}
	return positional, nil
}

// apiCommand parses args, authenticates and runs fn. org is honoured by the
// commands that work inside an organisation.
func apiCommand(args []string, n int, usage string, withOrg bool, fn func(ctx context.Context, c *client, args []string) error) error {
	f := newFlags(usage)
	var org *string
	if withOrg {
		org = f.String("org", "", "organisation ID")
	}
	positional, err := f.parse(args, n)
	if err != nil {
		return err
	}
	c, err := newClient(deref(org))
	if err != nil {
		return err
	}
	return fn(context.Background(), c, positional)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func orgsList(o *output, args []string) error {
	return apiCommand(args, 0, "orgs list", false, func(ctx context.Context, c *client, _ []string) error {
		orgs, err := c.organisations(ctx)
		if err != nil {
			return err
		}
		return o.result(orgs, func(w io.Writer) error {
			t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(t, "ID\tNAME\tROLE")
			for _, raw := range orgs {
				var org orgSummary
				if err := json.Unmarshal(raw, &org); err != nil {
					return err
				}
				fmt.Fprintf(t, "%s\t%s\t%s\n", org.ID, org.Name, org.Role)
			}
			return t.Flush()
		})
	})
}

type filesystem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

func fsList(o *output, args []string) error {
	return apiCommand(args, 0, "fs list [--org ORG_ID]", true, func(ctx context.Context, c *client, _ []string) error {
		raw, err := c.listFilesystems(ctx)
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var list []filesystem
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
			t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(t, "ID\tNAME\tCREATED")
			for _, fs := range list {
				fmt.Fprintf(t, "%s\t%s\t%s\n", fs.ID, fs.Name, fs.CreatedAt)
			}
			return t.Flush()
		})
	})
}

func fsCreate(o *output, args []string) error {
	return apiCommand(args, 1, "fs create NAME [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.createFilesystem(ctx, args[0])
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var fs filesystem
			if err := json.Unmarshal(raw, &fs); err != nil {
				return err
			}
			_, err := fmt.Fprintln(w, fs.ID)
			return err
		})
	})
}

func fsUsage(o *output, args []string) error {
	return apiCommand(args, 1, "fs usage FS_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.filesystemUsage(ctx, args[0])
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var u struct {
				RetainedBytes   int64 `json:"retained_bytes"`
				ByteQuota       int64 `json:"byte_quota"`
				Entries         int64 `json:"entries"`
				EntryQuota      int64 `json:"entry_quota"`
				BytesRead30d    int64 `json:"bytes_read_30d"`
				BytesWritten30d int64 `json:"bytes_written_30d"`
			}
			if err := json.Unmarshal(raw, &u); err != nil {
				return err
			}
			_, err := fmt.Fprintf(w, "stored   %s of %s\nfiles    %d of %d\nread     %s in 30 days\nwritten  %s in 30 days\n",
				humanBytes(u.RetainedBytes), humanBytes(u.ByteQuota), u.Entries, u.EntryQuota,
				humanBytes(u.BytesRead30d), humanBytes(u.BytesWritten30d))
			return err
		})
	})
}

// humanBytes formats n with a binary unit.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n)/unit, 0
	for value >= unit && exp < 4 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGTP"[exp])
}

func ticketCreate(o *output, args []string) error {
	f := newFlags("ticket create FS_ID [--ro] [--idempotency-key KEY]")
	readOnly := f.Bool("ro", false, "read-only")
	key := f.String("idempotency-key", "", "idempotency key")
	positional, err := f.parse(args, 1)
	if err != nil {
		return err
	}
	c, err := newClient("")
	if err != nil {
		return err
	}
	raw, err := c.createTicket(context.Background(), positional[0], *readOnly, *key, func(key string) {
		fmt.Fprintf(o.stderr, "mountable: idempotency key %s (retry with --idempotency-key %s)\n", key, key)
	})
	if err != nil {
		return err
	}
	return o.result(raw, func(w io.Writer) error {
		var s struct {
			ID              string `json:"id"`
			Mode            string `json:"mode"`
			Ticket          string `json:"ticket"`
			TicketExpiresAt string `json:"ticket_expires_at"`
			Replaced        string `json:"replaced_session_id"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if s.Replaced != "" {
			fmt.Fprintf(o.stderr, "mountable: revoked unused session %s, whose ticket was lost\n", s.Replaced)
		}
		fmt.Fprintf(o.stderr, "mountable: session %s (%s); the ticket works once, until %s\n", s.ID, s.Mode, s.TicketExpiresAt)
		_, err := fmt.Fprintln(w, s.Ticket)
		return err
	})
}

func sessionsList(o *output, args []string) error {
	return apiCommand(args, 1, "sessions list FS_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.listSessions(ctx, args[0])
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var list []struct {
				ID        string `json:"id"`
				Mode      string `json:"mode"`
				State     string `json:"state"`
				CreatedBy string `json:"created_by"`
				ExpiresAt string `json:"expires_at"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
			t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(t, "ID\tMODE\tSTATE\tCREATED BY\tEXPIRES")
			for _, s := range list {
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.Mode, s.State, s.CreatedBy, s.ExpiresAt)
			}
			return t.Flush()
		})
	})
}

func sessionsRevoke(o *output, args []string) error {
	return apiCommand(args, 1, "sessions revoke SESSION_ID", false, func(ctx context.Context, c *client, args []string) error {
		if err := c.revokeSession(ctx, args[0]); err != nil {
			return err
		}
		return o.result(map[string]any{"id": args[0], "revoked": true}, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "revoked %s\n", args[0])
			return err
		})
	})
}
