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

// stale makes a cache directory as a killed mount leaves it: marked, its
// lock free.
func stale(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cacheLockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// checkExists checks, for each path, whether it is still there.
func checkExists(t *testing.T, want map[string]bool) {
	t.Helper()
	for path, wanted := range want {
		if _, err := os.Lstat(path); (err == nil) != wanted {
			t.Errorf("%s: exists %v, want %v", filepath.Base(path), err == nil, wanted)
		}
	}
}

// A new mount removes the cache directories killed mounts left (marked,
// their lock free), and leaves a running mount's, an older CLI's
// (unmarked), and one whose lock file is a symbolic link.
func TestStaleCacheDirsArePruned(t *testing.T) {
	base := cacheBase(t)
	killed := stale(t, base, "mount-killed")
	running := stale(t, base, "mount-running")
	older := filepath.Join(base, "mount-older-cli")
	if err := os.Mkdir(older, 0o700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(base, "mount-linked")
	if err := os.Mkdir(linked, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "free-lock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linked, cacheLockName)); err != nil {
		t.Fatal(err)
	}
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
	checkExists(t, map[string]bool{killed: false, running: true, older: true, linked: true, dir: true})
	// Its own mark is held while it runs.
	other, err := os.OpenFile(filepath.Join(dir, cacheLockName), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("the new mount's lock is free")
	}
}

// When in doubt nothing is pruned, and the new directory stays unmarked
// so that no later mount takes it for a dead one: a cache root on a
// network filesystem, a busy root lock, a filesystem without flock.
func TestCacheDirsWhenInDoubt(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, base string)
	}{
		{"network filesystem", func(t *testing.T, _ string) {
			saved := localFilesystem
			localFilesystem = func(string) bool { return false }
			t.Cleanup(func() { localFilesystem = saved })
		}},
		{"busy root lock", func(t *testing.T, base string) {
			root, err := os.OpenFile(filepath.Join(base, cacheLockName), os.O_RDWR|os.O_CREATE, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { root.Close() })
			if err := unix.Flock(int(root.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
		}},
		{"no flock", func(t *testing.T, _ string) {
			saved := flock
			flock = func(int, int) error { return unix.ENOTSUP }
			t.Cleanup(func() { flock = saved })
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := cacheBase(t)
			killed := stale(t, base, "mount-killed")
			c.setup(t, base)
			dir, release, err := mountCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			checkExists(t, map[string]bool{killed: true, dir: true, filepath.Join(dir, cacheLockName): false})
		})
	}
}

// Mounts starting at once never prune each other's new directories.
func TestConcurrentCacheDirs(t *testing.T) {
	cacheBase(t)
	const n = 20
	type result struct {
		dir     string
		release func()
		err     error
	}
	results := make(chan result, n)
	for range n {
		go func() {
			dir, release, err := mountCacheDir()
			results <- result{dir, release, err}
		}()
	}
	var all []result
	for range n { // every worker, before anything can fail the test
		all = append(all, <-results)
	}
	for _, r := range all {
		if r.err == nil {
			defer r.release()
		}
	}
	for i, r := range all {
		if r.err != nil {
			t.Errorf("mount %d: %v", i, r.err)
		} else if _, err := os.Stat(r.dir); err != nil {
			t.Errorf("mount %d's directory: %v", i, err)
		}
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
