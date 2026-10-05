package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fakeWeed = "#!/bin/sh\necho fake weed\n"

// weedArchive is a release-shaped tar.gz holding one file, weed.
func weedArchive(t *testing.T) (archive []byte, sum string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "weed", Mode: 0o755, Size: int64(len(fakeWeed)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(fakeWeed)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(digest[:])
}

func serve(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureWeedInstallsVerifiedBinary(t *testing.T) {
	archive, sum := weedArchive(t)
	srv := serve(t, archive)
	path := filepath.Join(t.TempDir(), "mountable", "weed-4.48", "weed")

	got, err := ensureWeed(path, srv.URL+"/linux_amd64.tar.gz", sum)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != fakeWeed {
		t.Fatalf("content = %q", content)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
	assertDirHolds(t, filepath.Dir(path), []string{"weed"})
}

func TestEnsureWeedRejectsChecksumMismatch(t *testing.T) {
	archive, _ := weedArchive(t)
	srv := serve(t, archive)
	path := filepath.Join(t.TempDir(), "weed-4.48", "weed")

	_, err := ensureWeed(path, srv.URL+"/linux_amd64.tar.gz", strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
	assertDirHolds(t, filepath.Dir(path), nil)
}

func TestEnsureWeedReusesCachedBinary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected download of %s", r.URL.Path)
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "weed")
	if err := os.WriteFile(path, []byte("cached"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := ensureWeed(path, srv.URL+"/linux_amd64.tar.gz", strings.Repeat("0", 64))
	if err != nil || got != path {
		t.Fatalf("ensureWeed = %q, %v", got, err)
	}
}

func TestWeedBinaryHonoursOverride(t *testing.T) {
	t.Setenv("MOUNTABLE_WEED", "/opt/weed")
	if got, err := weedBinary(); err != nil || got != "/opt/weed" {
		t.Fatalf("weedBinary = %q, %v", got, err)
	}
}

// assertDirHolds fails unless dir holds exactly the names in want.
func assertDirHolds(t *testing.T, dir string, want []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("%s holds %v, want %v", dir, names, want)
	}
}
