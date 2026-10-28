//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCredentialsAreReplacedOwnerOnly(t *testing.T) {
	isolateConfig(t)
	t.Setenv("MOUNTABLE_API_URL", "http://127.0.0.1:1")
	path, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveCredentials(tokenResponse{AccessToken: testJWT, RefreshToken: "mtblrt_rotated", ExpiresIn: 900}, testGeneration); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}
	if c, err := loadCredentials(); err != nil || c.AccessToken != testJWT {
		t.Fatalf("loaded %+v, %v", c, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"credentials.json", "credentials.json.lock"}) {
		t.Fatalf("files = %v", names)
	}
}
