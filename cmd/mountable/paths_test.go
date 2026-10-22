package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

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

// A new mount removes the cache directories this host's killed mounts
// left (their lock is free), and leaves a running mount's, one made by an
// older CLI (no lock file), and one of another host sharing the home.
func TestStaleCacheDirsArePruned(t *testing.T) {
	base := cacheBase(t)
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	mkdir := func(name, owner string) string {
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if owner != "" {
			if err := os.WriteFile(filepath.Join(dir, cacheLockName), []byte(owner), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	killed := mkdir("mount-killed", host)
	running := mkdir("mount-running", host)
	older := mkdir("mount-older-cli", "")
	elsewhere := mkdir("mount-other-host", host+"-elsewhere")
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
	for path, want := range map[string]bool{killed: false, running: true, older: true, elsewhere: true, dir: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", filepath.Base(path), err == nil, want)
		}
	}
	// Its own lock names this host and is held while it runs.
	if owner, err := os.ReadFile(filepath.Join(dir, cacheLockName)); err != nil || string(owner) != host {
		t.Fatalf("lock file %q, %v", owner, err)
	}
	other, err := os.OpenFile(filepath.Join(dir, cacheLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("the new mount's lock is free")
	}
}

// Where the filesystem has no flock, mounting still works: nothing is
// locked or pruned, and the directory keeps no lock file, so no later
// mount takes it for a dead one.
func TestCacheDirWithoutFlock(t *testing.T) {
	base := cacheBase(t)
	saved := flock
	flock = func(int, int) error { return unix.ENOTSUP }
	t.Cleanup(func() { flock = saved })
	host, _ := os.Hostname()
	stale := filepath.Join(base, "mount-killed")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, cacheLockName), []byte(host), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, release, err := mountCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := os.Stat(filepath.Join(dir, cacheLockName)); !os.IsNotExist(err) {
		t.Fatalf("lock file: %v", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("a directory was pruned without locks")
	}
}

// cacheBase points the user cache directory at a fresh one and returns
// the mounts' base in it.
func cacheBase(t *testing.T) string {
	t.Helper()
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
	return base
}

// Mounts starting at once never prune each other's new directories: the
// root's lock covers creating, locking and pruning.
func TestConcurrentCacheDirs(t *testing.T) {
	cacheBase(t)
	const n = 20
	dirs := make([]string, n)
	releases := make([]func(), n)
	errs := make(chan error, n)
	for i := range n {
		go func() {
			var err error
			dirs[i], releases[i], err = mountCacheDir()
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	for i, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, cacheLockName)); err != nil {
			t.Errorf("mount %d's directory: %v", i, err)
		}
		releases[i]()
	}
}
