package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalDir(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	mnt := filepath.Join(real, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Symlink(real, filepath.Join(base, "parent-link")))
	must(os.Symlink(mnt, filepath.Join(base, "leaf-link")))
	must(os.Symlink("real/mnt", filepath.Join(base, "relative-link")))
	t.Chdir(real)

	for _, dir := range []string{
		"mnt", "./mnt/", "../real/mnt", mnt,
		filepath.Join(base, "parent-link", "mnt"),
		filepath.Join(base, "leaf-link"),
		filepath.Join(base, "relative-link"),
	} {
		got, err := canonicalDir(dir)
		if err != nil || got != mnt {
			t.Errorf("canonicalDir(%q) = %q, %v; want %q", dir, got, err, mnt)
		}
	}
}

func TestCanonicalDirLoop(t *testing.T) {
	base := t.TempDir()
	loop := filepath.Join(base, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := canonicalDir(loop); err == nil {
		t.Fatal("a symlink loop was accepted")
	}
}
