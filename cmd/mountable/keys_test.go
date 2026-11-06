package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

const (
	testManagementKey = "mtblm_testmanagementsecret"
	newKeySecret      = "mtbl_newkeysecret"
	newS3Secret       = "c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0c2VjcmV0"
	apiKeyCreated     = `{"id":"k1","name":"worker","prefix":"mtbl_abcdefg","filesystem_id":"fsone","expires_at":"2027-01-01T00:00:00Z","revoked":false,"created_at":"2026-11-06T00:00:00Z","secret":"` + newKeySecret + `"}`
	apiKeys           = `[{"id":"k1","name":"worker","prefix":"mtbl_abcdefg","filesystem_id":null,"expires_at":"2027-01-01T00:00:00Z","revoked":true,"created_at":"2026-11-06T00:00:00Z"}]`
	s3KeyCreated      = `{"access_key_id":"MTBLABCDEFGHIJKLMNOP","filesystem_id":"fsone","name":"duck","mode":"ro","expires_at":"2027-01-01T00:00:00Z","revoked":false,"created_at":"2026-11-06T00:00:00Z","secret_access_key":"` + newS3Secret + `"}`
	s3Keys            = `[{"access_key_id":"MTBLABCDEFGHIJKLMNOP","filesystem_id":"fsone","name":"duck","mode":"ro","expires_at":"2027-01-01T00:00:00Z","revoked":false,"created_at":"2026-11-06T00:00:00Z"}]`
	grant             = `{"principal_type":"api_key","principal_id":"k1","mode":"ro","id":"g1"}`
)

func useManagementKey(t *testing.T) {
	t.Setenv("MOUNTABLE_API_KEY", testManagementKey)
	isolateConfig(t)
}

// runManaged runs the CLI and fails if the management key leaks.
func runManaged(t *testing.T, args ...string) result {
	t.Helper()
	r := runCLI(t, args...)
	if strings.Contains(r.stdout+r.stderr, testManagementKey) {
		t.Fatalf("%v leaked the management key: %q %q", args, r.stdout, r.stderr)
	}
	return r
}

func TestFilesystemDelete(t *testing.T) {
	useManagementKey(t)
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"DELETE /api/v1/orgs/" + orgA + "/filesystems/fsone": reply(204, ""),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)
	r := runManaged(t, "fs", "delete", "fsone", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != `{"deleted":true,"id":"fsone"}` {
		t.Fatalf("fs delete --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	r = runManaged(t, "fs", "delete", "fsone")
	if r.code != 0 || r.stdout != "deleted fsone\n" {
		t.Fatalf("fs delete = %d %q", r.code, r.stdout)
	}
	if api.auth[0] != "Bearer "+testManagementKey {
		t.Errorf("authorization = %q", api.auth[0])
	}
	// filesystems/.. would be the organisation itself.
	for _, bad := range []string{"", ".", ".."} {
		r = runManaged(t, "fs", "delete", bad, "--json")
		if e := jsonError(t, r); r.code != 1 || e.Code != "invalid_request" {
			t.Errorf("fs delete %q = %d %+v", bad, r.code, e)
		}
	}
	// So would an organisation ID of "..".
	t.Setenv("MOUNTABLE_ORG", "..")
	if e := jsonError(t, runManaged(t, "fs", "delete", "fsone", "--json")); e.Code != "invalid_request" {
		t.Errorf("MOUNTABLE_ORG=.. = %+v", e)
	}
	if calls := api.called(); len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestAPIKeyCommands(t *testing.T) {
	useManagementKey(t)
	keys := "/api/v1/orgs/" + orgA + "/api-keys"
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST " + keys:           reply(201, apiKeyCreated),
		"GET " + keys:            reply(200, apiKeys),
		"DELETE " + keys + "/k1": reply(204, ""),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)

	r := runManaged(t, "api-keys", "create", "worker", "--filesystem", "fsone", "--ro", "--expires-in-days", "7", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != apiKeyCreated || r.stderr != "" {
		t.Fatalf("api-keys create --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	want := map[string]any{"name": "worker", "filesystem_id": "fsone", "mode": "ro", "expires_in_days": float64(7)}
	if got, _ := json.Marshal(api.json[0]); string(got) != string(must(json.Marshal(want))) {
		t.Errorf("create body = %s", got)
	}
	r = runManaged(t, "api-keys", "create", "worker")
	if r.code != 0 || r.stdout != newKeySecret+"\n" || !strings.Contains(r.stderr, "k1") || !strings.Contains(r.stderr, "only once") || strings.Contains(r.stderr, newKeySecret) {
		t.Fatalf("api-keys create = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if got, _ := json.Marshal(api.json[1]); string(got) != `{"name":"worker"}` {
		t.Errorf("create body without flags = %s", got)
	}
	// A mode applies only to a key limited to one filesystem.
	if r = runManaged(t, "api-keys", "create", "worker", "--ro"); r.code != 2 {
		t.Fatalf("--ro without --filesystem = %d %q", r.code, r.stderr)
	}

	r = runManaged(t, "api-keys", "list", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != apiKeys {
		t.Fatalf("api-keys list --json = %d %q", r.code, r.stdout)
	}
	r = runManaged(t, "api-keys", "list")
	wantList := "ID  NAME    FILESYSTEM  EXPIRES               REVOKED\n" +
		"k1  worker  -           2027-01-01T00:00:00Z  true\n"
	if r.code != 0 || r.stdout != wantList {
		t.Fatalf("api-keys list = %d %q", r.code, r.stdout)
	}
	r = runManaged(t, "api-keys", "revoke", "k1", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != `{"id":"k1","revoked":true}` {
		t.Fatalf("api-keys revoke --json = %d %q", r.code, r.stdout)
	}
	if calls := api.called(); calls[len(calls)-1] != "DELETE "+keys+"/k1" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestS3KeyCommands(t *testing.T) {
	useManagementKey(t)
	keys := "/api/v1/orgs/" + orgA + "/filesystems/fsone/s3-keys"
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST " + keys: reply(201, s3KeyCreated),
		"GET " + keys:  reply(200, s3Keys),
		"DELETE " + keys + "/MTBLABCDEFGHIJKLMNOP": reply(204, ""),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)

	r := runManaged(t, "s3-keys", "create", "fsone", "duck", "--ro", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != s3KeyCreated || r.stderr != "" {
		t.Fatalf("s3-keys create --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if got, _ := json.Marshal(api.json[0]); string(got) != `{"mode":"ro","name":"duck"}` {
		t.Errorf("create body = %s", got)
	}
	r = runManaged(t, "s3-keys", "create", "fsone", "duck", "--expires-in-days", "365")
	if r.code != 0 || r.stdout != newS3Secret+"\n" || !strings.Contains(r.stderr, "MTBLABCDEFGHIJKLMNOP") || strings.Contains(r.stderr, newS3Secret) {
		t.Fatalf("s3-keys create = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if got, _ := json.Marshal(api.json[1]); string(got) != `{"expires_in_days":365,"mode":"rw","name":"duck"}` {
		t.Errorf("create body = %s", got)
	}
	r = runManaged(t, "s3-keys", "list", "fsone")
	want := "ACCESS KEY ID         NAME  MODE  EXPIRES               REVOKED\n" +
		"MTBLABCDEFGHIJKLMNOP  duck  ro    2027-01-01T00:00:00Z  false\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("s3-keys list = %d %q", r.code, r.stdout)
	}
	r = runManaged(t, "s3-keys", "revoke", "fsone", "MTBLABCDEFGHIJKLMNOP")
	if r.code != 0 || r.stdout != "revoked MTBLABCDEFGHIJKLMNOP\n" {
		t.Fatalf("s3-keys revoke = %d %q", r.code, r.stdout)
	}
}

func TestGrantCommands(t *testing.T) {
	useManagementKey(t)
	grants := "/api/v1/orgs/" + orgA + "/filesystems/fsone/grants"
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"PUT " + grants:            reply(200, grant),
		"GET " + grants:            reply(200, "["+grant+"]"),
		"DELETE " + grants + "/g1": reply(204, ""),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)

	r := runManaged(t, "grants", "set", "fsone", "api_key", "k1", "ro", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != grant {
		t.Fatalf("grants set --json = %d %q", r.code, r.stdout)
	}
	if got, _ := json.Marshal(api.json[0]); string(got) != `{"mode":"ro","principal_id":"k1","principal_type":"api_key"}` {
		t.Errorf("set body = %s", got)
	}
	r = runManaged(t, "grants", "list", "fsone")
	if want := "ID  TYPE     PRINCIPAL  MODE\ng1  api_key  k1         ro\n"; r.code != 0 || r.stdout != want {
		t.Fatalf("grants list = %d %q", r.code, r.stdout)
	}
	r = runManaged(t, "grants", "delete", "fsone", "g1")
	if r.code != 0 || r.stdout != "deleted g1\n" {
		t.Fatalf("grants delete = %d %q", r.code, r.stdout)
	}
	if r = runManaged(t, "grants", "set", "fsone", "api_key", "k1"); r.code != 2 {
		t.Fatalf("grants set without a mode = %d", r.code)
	}
}

func TestKeyCreateUnknownOutcome(t *testing.T) {
	useManagementKey(t)
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/orgs/" + orgA + "/api-keys":                  reply(502, "bad gateway"),
		"POST /api/v1/orgs/" + orgA + "/filesystems/fsone/s3-keys": hangUp,
		"POST /api/v1/orgs/" + orgB + "/api-keys":                  reply(403, `{"detail":{"code":"management_key_required"}}`),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)
	for _, args := range [][]string{{"api-keys", "create", "worker"}, {"s3-keys", "create", "fsone", "duck"}} {
		e := jsonError(t, runManaged(t, append(args, "--json")...))
		if !strings.Contains(e.Hint, "may have been created") || !strings.Contains(e.Hint, "revoke") {
			t.Errorf("%v: %+v", args, e)
		}
	}
	// A refusal is not an unknown outcome.
	e := jsonError(t, runManaged(t, "api-keys", "create", "worker", "--org", orgB, "--json"))
	if e.Code != "management_key_required" || !strings.Contains(e.Hint, "management key") {
		t.Errorf("refusal: %+v", e)
	}
}

func TestKeyDotSegmentIDsAreRefused(t *testing.T) {
	useManagementKey(t)
	api := newFakeAPI(t, nil) // no MOUNTABLE_ORG: not even /me is asked
	for _, bad := range []string{"", ".", ".."} {
		for _, args := range [][]string{
			{"api-keys", "revoke", bad},
			{"s3-keys", "list", bad},
			{"s3-keys", "create", bad, "duck"},
			{"s3-keys", "revoke", "fsone", bad},
			{"grants", "list", bad},
			{"grants", "set", bad, "api_key", "k1", "ro"},
			{"grants", "delete", "fsone", bad},
		} {
			r := runManaged(t, append(args, "--json")...)
			if e := jsonError(t, r); r.code != 1 || e.Code != "invalid_request" {
				t.Errorf("%v = %d %+v", args, r.code, e)
			}
		}
	}
	if calls := api.called(); len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
}

func must(data []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return data
}
