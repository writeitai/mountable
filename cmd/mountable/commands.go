package main

// The management commands: organisations, filesystems, tickets, sessions,
// webhooks.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"
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
		note(o.stderr, "%s (%s)", e.Message, e.Code)
		if e.Hint != "" {
			note(o.stderr, "hint: %s", e.Hint)
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

// given names the flags that were set.
func (f *flags) given() map[string]bool {
	set := map[string]bool{}
	f.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	return set
}

// apiCommand parses args, authenticates and runs fn. org is honoured by the
// commands that work inside an organisation.
func apiCommand(args []string, n int, usage string, withOrg bool, fn func(ctx context.Context, c *client, args []string) error) error {
	return apiCommandFlags(newFlags(usage), args, n, withOrg, fn)
}

// apiCommandFlags is apiCommand with f, to which the command added its own
// flags.
func apiCommandFlags(f *flags, args []string, n int, withOrg bool, fn func(ctx context.Context, c *client, args []string) error) error {
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
	ID                 string `json:"id"`
	Name               string `json:"name"`
	CreatedAt          string `json:"created_at"`
	ByteQuota          int64  `json:"byte_quota"`
	EntryQuota         int64  `json:"entry_quota"`
	SpendingCapCents   *int64 `json:"spending_cap_cents"`
	MutationsPerMinute *int64 `json:"mutations_per_minute"`
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

const (
	fsCreateUsage = "fs create NAME [--spending-cap-cents N] [--mutations-per-minute N] [--byte-quota BYTES] [--entry-quota N] [--org ORG_ID]"
	fsUpdateUsage = "fs update FS_ID [--spending-cap-cents N|none] [--mutations-per-minute N|none] [--byte-quota BYTES] [--entry-quota N] [--org ORG_ID]"
)

// limitValue is a limit flag. It parses as a non-negative integer, or, when
// nullable, as none (sent as null); the API checks the ranges.
type limitValue struct {
	field    string
	nullable bool
	limits   map[string]any
}

func (v *limitValue) String() string { return "" }

func (v *limitValue) Set(s string) error {
	if s == "none" && v.nullable {
		v.limits[v.field] = nil
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 63)
	if err != nil {
		return err
	}
	v.limits[v.field] = n
	return nil
}

// addLimitFlags adds the limit flags to f and returns the limits given, as
// API fields, once f has parsed. With update, none removes the spending cap
// or restores the default mutation rate; the quotas cannot be removed.
func addLimitFlags(f *flags, update bool) map[string]any {
	limits := map[string]any{}
	for _, l := range []struct {
		name, field string
		nullable    bool
	}{
		{"spending-cap-cents", "spending_cap_cents", true},
		{"mutations-per-minute", "mutations_per_minute", true},
		{"byte-quota", "byte_quota", false},
		{"entry-quota", "entry_quota", false},
	} {
		f.Var(&limitValue{field: l.field, nullable: update && l.nullable, limits: limits}, l.name, l.field)
	}
	return limits
}

func fsCreate(o *output, args []string) error {
	f := newFlags(fsCreateUsage)
	limits := addLimitFlags(f, false)
	return apiCommandFlags(f, args, 1, true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.createFilesystem(ctx, args[0], limits)
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

func fsGet(o *output, args []string) error {
	return apiCommand(args, 1, "fs get FS_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.getFilesystem(ctx, args[0])
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error { return printFilesystem(w, raw) })
	})
}

// fsUpdate sends only the limits given; at least one is required.
func fsUpdate(o *output, args []string) error {
	f := newFlags(fsUpdateUsage)
	org := f.String("org", "", "organisation ID")
	limits := addLimitFlags(f, true)
	positional, err := f.parse(args, 1)
	if err != nil {
		return err
	}
	if len(limits) == 0 {
		return usageError(fsUpdateUsage)
	}
	c, err := newClient(*org)
	if err != nil {
		return err
	}
	raw, err := c.updateFilesystem(context.Background(), positional[0], limits)
	if err != nil {
		return err
	}
	return o.result(raw, func(w io.Writer) error { return printFilesystem(w, raw) })
}

// printFilesystem writes a filesystem and its limits for people.
func printFilesystem(w io.Writer, raw json.RawMessage) error {
	var fs filesystem
	if err := json.Unmarshal(raw, &fs); err != nil {
		return err
	}
	orNone := func(n *int64, unit string) string {
		if n == nil {
			return "none"
		}
		return strconv.FormatInt(*n, 10) + unit
	}
	t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(t, "id\t%s\nname\t%s\ncreated\t%s\n", fs.ID, fs.Name, fs.CreatedAt)
	fmt.Fprintf(t, "spending cap\t%s\nmutations per minute\t%s\n", orNone(fs.SpendingCapCents, " cents"), orNone(fs.MutationsPerMinute, ""))
	fmt.Fprintf(t, "byte quota\t%s\nentry quota\t%d\n", humanBytes(fs.ByteQuota), fs.EntryQuota)
	return t.Flush()
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

const ticketUsage = "ticket create FS_ID [--ro] [--idempotency-key KEY] [--max-duration DURATION]"

func ticketCreate(o *output, args []string) error {
	f := newFlags(ticketUsage)
	readOnly := f.Bool("ro", false, "read-only")
	key := f.String("idempotency-key", "", "idempotency key")
	maxDuration := f.Duration("max-duration", 0, "the session's maximum duration, such as 12h or 720h")
	positional, err := f.parse(args, 1)
	if err != nil {
		return err
	}
	// A duration is sent in whole seconds. Without the flag the API's default
	// applies; a duration given must be positive.
	given := false
	f.Visit(func(fl *flag.Flag) { given = given || fl.Name == "max-duration" })
	if given && (*maxDuration <= 0 || *maxDuration%time.Second != 0) {
		return usageError(ticketUsage)
	}
	c, err := newClient("")
	if err != nil {
		return err
	}
	raw, err := c.createTicket(context.Background(), positional[0], *readOnly, *key, int64(*maxDuration/time.Second), func(key string) {
		note(o.stderr, "idempotency key %s (retry with --idempotency-key %s)", key, key)
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
			note(o.stderr, "revoked unused session %s, whose ticket was lost", s.Replaced)
		}
		note(o.stderr, "session %s (%s); the ticket works once, until %s", s.ID, s.Mode, s.TicketExpiresAt)
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

func webhooksCreate(o *output, args []string) error {
	f := newFlags("webhooks create FS_ID URL [--path-prefix PREFIX] [--org ORG_ID]")
	prefix := f.String("path-prefix", "", "only changes under this path")
	return apiCommandFlags(f, args, 2, true, func(ctx context.Context, c *client, args []string) error {
		// A prefix given empty is sent, and the API refuses it.
		var pathPrefix *string
		if f.given()["path-prefix"] {
			pathPrefix = prefix
		}
		raw, err := c.createWebhook(ctx, args[0], args[1], pathPrefix)
		if err != nil {
			return err
		}
		return printSecret(o, raw, "webhook %s created")
	})
}

func webhooksRotateSecret(o *output, args []string) error {
	return apiCommand(args, 2, "webhooks rotate-secret FS_ID WEBHOOK_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.webhook(ctx, "POST", args[0], args[1], ":rotate-secret")
		if err != nil {
			return err
		}
		return printSecret(o, raw, "new secret for webhook %s")
	})
}

// printSecret writes a webhook's signing secret alone on stdout, and what
// it belongs to on stderr; with --json, the API's answer.
func printSecret(o *output, raw json.RawMessage, what string) error {
	return o.result(raw, func(w io.Writer) error {
		var hook struct {
			ID     string `json:"id"`
			Secret string `json:"secret"`
		}
		if err := json.Unmarshal(raw, &hook); err != nil {
			return err
		}
		note(o.stderr, what+"; the secret is shown only once, so store it now", hook.ID)
		_, err := fmt.Fprintln(w, hook.Secret)
		return err
	})
}

func webhooksList(o *output, args []string) error {
	return apiCommand(args, 1, "webhooks list FS_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.listWebhooks(ctx, args[0])
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var list []struct {
				ID          string  `json:"id"`
				URL         string  `json:"url"`
				PathPrefix  string  `json:"path_prefix"`
				State       string  `json:"state"`
				LastFailure *string `json:"last_failure"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
			t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(t, "ID\tSTATE\tPATH-PREFIX\tURL\tLAST-FAILURE")
			for _, h := range list {
				failure := "-"
				if h.LastFailure != nil {
					failure = *h.LastFailure
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", h.ID, h.State, h.PathPrefix, h.URL, failure)
			}
			return t.Flush()
		})
	})
}

func webhooksDelete(o *output, args []string) error {
	return apiCommand(args, 2, "webhooks delete FS_ID WEBHOOK_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		if _, err := c.webhook(ctx, "DELETE", args[0], args[1], ""); err != nil {
			return err
		}
		return o.result(map[string]any{"id": args[1], "deleted": true}, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "deleted %s\n", args[1])
			return err
		})
	})
}

func webhooksEnable(o *output, args []string) error {
	return apiCommand(args, 2, "webhooks enable FS_ID WEBHOOK_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.webhook(ctx, "POST", args[0], args[1], ":enable")
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var hook struct {
				ID    string `json:"id"`
				State string `json:"state"`
			}
			if err := json.Unmarshal(raw, &hook); err != nil {
				return err
			}
			_, err := fmt.Fprintf(w, "%s %s\n", hook.ID, hook.State)
			return err
		})
	})
}
