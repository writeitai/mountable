package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
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

// Without a user cache directory, a mount's cache goes in
// $TMPDIR/mountable-<uid>, which must be this user's alone: a symlink or a
// directory others can write is refused before anything goes in it.
func TestCacheDirFallback(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("TMPDIR", tmp)
	base := filepath.Join(tmp, fmt.Sprintf("mountable-%d", os.Getuid()))

	dir, release, err := mountCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	release()
	if filepath.Dir(dir) != base {
		t.Fatalf("cache directory %s, want one in %s", dir, base)
	}
	for _, d := range []string{base, dir} {
		if info, err := os.Lstat(d); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, info.Mode(), err)
		}
	}
	os.RemoveAll(base)

	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, base); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mountCacheDir(); err == nil {
		t.Fatal("a symlinked fallback directory was used")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatal("something was created through the symlink")
	}
	os.Remove(base)

	if err := os.Mkdir(base, 0o777); err != nil {
		t.Fatal(err)
	}
	os.Chmod(base, 0o777) // whatever the umask
	if _, _, err := mountCacheDir(); err == nil {
		t.Fatal("a fallback directory others can write was used")
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Fatal("something was created in it")
	}
}

// A new mount removes the cache directories of mounts whose process ended
// without removing them (their lock is free), and leaves a running mount's,
// one made by an older CLI (no lock) and one just made.
func TestStaleCacheDirsArePruned(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache) // Linux
	t.Setenv("HOME", cache)           // macOS: ~/Library/Caches
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	base = filepath.Join(base, "mountable")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * cacheGrace)
	mkdir := func(name string, lock bool, at time.Time) string {
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if lock {
			if err := os.WriteFile(filepath.Join(dir, cacheLockName), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	killed := mkdir("mount-killed", true, old)
	running := mkdir("mount-running", true, old)
	older := mkdir("mount-older-cli", false, old)
	fresh := mkdir("mount-fresh", true, time.Now())
	held, err := os.OpenFile(filepath.Join(running, cacheLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	dir, release, err := mountCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for path, want := range map[string]bool{killed: false, running: true, older: true, fresh: true, dir: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", filepath.Base(path), err == nil, want)
		}
	}
	// Its own lock is held while it runs.
	other, err := os.OpenFile(filepath.Join(dir, cacheLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("the new mount's lock is free")
	}
}
