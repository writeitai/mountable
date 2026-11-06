package main

// The key and grant commands (D60): API keys, S3 keys and grants, for a
// person who is an owner or admin, or a management key. Creating a key
// prints its secret alone on stdout, once.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
)

// unknownOutcome gives err the hint unknown when the request's outcome is
// unknown: the request or its answer was lost, or the API failed with a 5xx.
func unknownOutcome(err error, unknown string) error {
	var api *apiError
	var own *cliError
	if errors.As(err, &own) || (errors.As(err, &api) && api.Status < 500) {
		return err
	}
	e := toCLIError(err)
	e.Hint = unknown
	return e
}

// keyCreateFlags adds the flags every key create command takes. The values
// given are sent; the API applies its defaults to the others and checks the
// ranges.
func keyCreateFlags(f *flags) (expires *int, readOnly *bool) {
	return f.Int("expires-in-days", 0, "days until the key expires (API default 30, at most 365)"),
		f.Bool("ro", false, "read-only")
}

const apiKeysCreateUsage = "api-keys create NAME [--expires-in-days N] [--filesystem FS_ID [--ro]] [--org ORG_ID]"

func apiKeysCreate(o *output, args []string) error {
	f := newFlags(apiKeysCreateUsage)
	expires, readOnly := keyCreateFlags(f)
	fsID := f.String("filesystem", "", "limit the key to this filesystem")
	return apiCommandFlags(f, args, 1, true, func(ctx context.Context, c *client, args []string) error {
		given := f.given()
		// A mode applies only to a key limited to one filesystem.
		if *readOnly && !given["filesystem"] {
			return usageError(apiKeysCreateUsage)
		}
		body := map[string]any{"name": args[0]}
		if given["expires-in-days"] {
			body["expires_in_days"] = *expires
		}
		if given["filesystem"] {
			body["filesystem_id"] = *fsID
			body["mode"] = mode(*readOnly)
		}
		if err := c.resolveOrg(ctx); err != nil {
			return err
		}
		raw, err := c.inOrg(ctx, "POST", "/api-keys", body)
		if err != nil {
			return unknownOutcome(err, "the key may have been created, and its secret is not shown again: run `mountable api-keys list`, revoke a key with this name, then create a new one")
		}
		return printKeySecret(o, raw, "id", "secret", "API key %s created")
	})
}

func apiKeysList(o *output, args []string) error {
	return apiCommand(args, 0, "api-keys list [--org ORG_ID]", true, func(ctx context.Context, c *client, _ []string) error {
		raw, err := c.inOrg(ctx, "GET", "/api-keys", nil)
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var list []struct {
				ID           string  `json:"id"`
				Name         string  `json:"name"`
				FilesystemID *string `json:"filesystem_id"`
				ExpiresAt    string  `json:"expires_at"`
				Revoked      bool    `json:"revoked"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
			t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(t, "ID\tNAME\tFILESYSTEM\tEXPIRES\tREVOKED")
			for _, k := range list {
				fs := "-"
				if k.FilesystemID != nil {
					fs = *k.FilesystemID
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%t\n", k.ID, k.Name, fs, k.ExpiresAt, k.Revoked)
			}
			return t.Flush()
		})
	})
}

func apiKeysRevoke(o *output, args []string) error {
	return apiCommand(args, 1, "api-keys revoke KEY_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		if _, err := c.inOrgAt(ctx, "DELETE", nil, "api-keys", args[0]); err != nil {
			return err
		}
		return printRevoked(o, args[0])
	})
}

const s3KeysCreateUsage = "s3-keys create FS_ID NAME [--ro] [--expires-in-days N] [--org ORG_ID]"

func s3KeysCreate(o *output, args []string) error {
	f := newFlags(s3KeysCreateUsage)
	expires, readOnly := keyCreateFlags(f)
	return apiCommandFlags(f, args, 2, true, func(ctx context.Context, c *client, args []string) error {
		path, err := idPath("filesystems", args[0], "s3-keys")
		if err != nil {
			return err
		}
		body := map[string]any{"name": args[1], "mode": mode(*readOnly)}
		if f.given()["expires-in-days"] {
			body["expires_in_days"] = *expires
		}
		if err := c.resolveOrg(ctx); err != nil {
			return err
		}
		raw, err := c.inOrg(ctx, "POST", path, body)
		if err != nil {
			return unknownOutcome(err, "the key may have been created, and its secret is not shown again: run `mountable s3-keys list FS_ID`, revoke a key with this name, then create a new one")
		}
		return printKeySecret(o, raw, "access_key_id", "secret_access_key", "S3 key %s created (its access key ID)")
	})
}

func s3KeysList(o *output, args []string) error {
	return apiCommand(args, 1, "s3-keys list FS_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.inOrgAt(ctx, "GET", nil, "filesystems", args[0], "s3-keys")
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var list []struct {
				AccessKeyID string `json:"access_key_id"`
				Name        string `json:"name"`
				Mode        string `json:"mode"`
				ExpiresAt   string `json:"expires_at"`
				Revoked     bool   `json:"revoked"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return err
			}
			t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(t, "ACCESS KEY ID\tNAME\tMODE\tEXPIRES\tREVOKED")
			for _, k := range list {
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%t\n", k.AccessKeyID, k.Name, k.Mode, k.ExpiresAt, k.Revoked)
			}
			return t.Flush()
		})
	})
}

func s3KeysRevoke(o *output, args []string) error {
	return apiCommand(args, 2, "s3-keys revoke FS_ID ACCESS_KEY_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		if _, err := c.inOrgAt(ctx, "DELETE", nil, "filesystems", args[0], "s3-keys", args[1]); err != nil {
			return err
		}
		return printRevoked(o, args[1])
	})
}

func grantsList(o *output, args []string) error {
	return apiCommand(args, 1, "grants list FS_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		raw, err := c.inOrgAt(ctx, "GET", nil, "filesystems", args[0], "grants")
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error { return printGrants(w, raw, true) })
	})
}

// grantsSet creates a principal's grant, or changes its mode; the API checks
// the principal type and the mode.
func grantsSet(o *output, args []string) error {
	return apiCommand(args, 4, "grants set FS_ID member|group|api_key PRINCIPAL_ID ro|rw [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		body := map[string]string{"principal_type": args[1], "principal_id": args[2], "mode": args[3]}
		raw, err := c.inOrgAt(ctx, "PUT", body, "filesystems", args[0], "grants")
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error { return printGrants(w, json.RawMessage("["+string(raw)+"]"), false) })
	})
}

func grantsDelete(o *output, args []string) error {
	return apiCommand(args, 2, "grants delete FS_ID GRANT_ID [--org ORG_ID]", true, func(ctx context.Context, c *client, args []string) error {
		if _, err := c.inOrgAt(ctx, "DELETE", nil, "filesystems", args[0], "grants", args[1]); err != nil {
			return err
		}
		return o.result(map[string]any{"id": args[1], "deleted": true}, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "deleted %s\n", args[1])
			return err
		})
	})
}

func printGrants(w io.Writer, raw json.RawMessage, header bool) error {
	var list []struct {
		ID            string `json:"id"`
		PrincipalType string `json:"principal_type"`
		PrincipalID   string `json:"principal_id"`
		Mode          string `json:"mode"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if header {
		fmt.Fprintln(t, "ID\tTYPE\tPRINCIPAL\tMODE")
	}
	for _, g := range list {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", g.ID, g.PrincipalType, g.PrincipalID, g.Mode)
	}
	return t.Flush()
}

func mode(readOnly bool) string {
	if readOnly {
		return "ro"
	}
	return "rw"
}

// printKeySecret writes a new key's secret alone on stdout and its ID on
// stderr; with --json, the API's answer.
func printKeySecret(o *output, raw json.RawMessage, idField, secretField, what string) error {
	return o.result(raw, func(w io.Writer) error {
		var key map[string]any
		if err := json.Unmarshal(raw, &key); err != nil {
			return err
		}
		secret, _ := key[secretField].(string)
		note(o.stderr, what+"; the secret is shown only once, so store it now", key[idField])
		_, err := fmt.Fprintln(w, secret)
		return err
	})
}

func printRevoked(o *output, id string) error {
	return o.result(map[string]any{"id": id, "revoked": true}, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "revoked %s\n", id)
		return err
	})
}
