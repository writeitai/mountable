package nfs

import (
	"fmt"
	"slices"
	"syscall"
	"testing"
)

type listing struct {
	verf    uint64
	entries []pageEntry
	eof     bool
	size    int
}

type pageEntry struct {
	fileid, cookie uint64
	name           string
	attr           *fattr
	fh             []byte
}

func (l listing) names() []string {
	var names []string
	for _, e := range l.entries {
		names = append(names, e.name)
	}
	return names
}

func (l listing) last() uint64 { return l.entries[len(l.entries)-1].cookie }

// readDirPlus reads one READDIRPLUS page.
func (c *testClient) readDirPlus(dir []byte, cookie, verf uint64, dirCount, maxCount uint32) (uint32, listing) {
	c.t.Helper()
	r := c.call(procReadDirPlus, wire{}.opaque(dir).u64(cookie).u64(verf).u32(dirCount).u32(maxCount))
	size := len(r.b)
	status := r.u32()
	if _, ok := r.postOpAttr(); !ok {
		c.t.Fatal("no directory attributes")
	}
	if status != 0 {
		return status, listing{}
	}
	l := listing{verf: r.u64(), size: size}
	for r.u32() == 1 {
		e := pageEntry{fileid: r.u64(), name: string(r.opaque()), cookie: r.u64()}
		if a, ok := r.postOpAttr(); ok {
			e.attr = &a
		}
		if r.u32() == 1 {
			e.fh = r.opaque()
		}
		l.entries = append(l.entries, e)
	}
	l.eof = r.u32() == 1
	if len(r.b) != 0 {
		c.t.Fatalf("%d bytes after the page", len(r.b))
	}
	if size > int(maxCount) {
		c.t.Fatalf("a page of %d bytes for maxcount %d", size, maxCount)
	}
	return status, l
}

// readAll lists dir from cookie, in pages of maxCount, to the end.
func (c *testClient) readAll(dir []byte, cookie, verf uint64, maxCount uint32) []string {
	c.t.Helper()
	var names []string
	for {
		status, p := c.readDirPlus(dir, cookie, verf, maxCount, maxCount)
		if status != 0 {
			c.t.Fatalf("status %d from cookie %d", status, cookie)
		}
		names = append(names, p.names()...)
		if p.eof {
			return names
		}
		cookie, verf = p.last(), p.verf
	}
}

func wideEngine(n int) *fakeEngine {
	f := newFakeEngine()
	f.add("wide", 5000, syscall.S_IFDIR|0o755, "")
	for i := range n {
		f.add(fmt.Sprintf("wide/file-%04d", i), uint64(6000+i), syscall.S_IFREG|0o644, "x")
	}
	return f
}

func wideNames(n int) []string {
	names := []string{".", ".."}
	for i := range n {
		names = append(names, fmt.Sprintf("file-%04d", i))
	}
	return names
}

// A directory read in small pages yields every entry once, each with
// attributes and a handle, within both budgets.
func TestReadDirPlusPages(t *testing.T) {
	f := wideEngine(50)
	_, c := startServer(t, f)
	root := c.root()
	dir, dirAttr := c.lookup(root, "wide")
	var cookie, verf uint64
	var names []string
	pages := 0
	for {
		status, p := c.readDirPlus(dir, cookie, verf, 1024, 2048)
		if status != 0 {
			t.Fatalf("page %d: status %d", pages, status)
		}
		if pages > 0 && p.verf != verf {
			t.Fatal("the verifier changed between pages")
		}
		pages++
		for _, e := range p.entries {
			names = append(names, e.name)
			if e.attr == nil || len(e.fh) != handleSize || e.fileid == 0 || e.attr.fileid != e.fileid {
				t.Fatalf("entry %+v", e)
			}
			if e.name == "." && e.fileid != dirAttr.fileid || e.name == ".." && e.fileid != rootInode {
				t.Fatalf("%s has fileid %d", e.name, e.fileid)
			}
		}
		if p.eof {
			break
		}
		cookie, verf = p.last(), p.verf
	}
	if !slices.Equal(names, wideNames(50)) || pages < 3 {
		t.Fatalf("%d pages listed %v", pages, names)
	}
	// Every child holds one reference however its handle was handed out.
	fh, _ := c.lookup(dir, "file-0007")
	if n := f.refs("wide/file-0007"); n != 1 {
		t.Fatalf("%d references", n)
	}
	if s := c.status(procGetAttr, wire{}.opaque(fh)); s != 0 {
		t.Fatalf("GETATTR of a listed handle: %d", s)
	}
}

// dircount bounds a page even when maxcount would allow more.
func TestReadDirPlusHonoursDirCount(t *testing.T) {
	_, c := startServer(t, wideEngine(50))
	dir, _ := c.lookup(c.root(), "wide")
	_, p := c.readDirPlus(dir, 0, 0, 200, 1<<20)
	used := 0
	for _, e := range p.entries {
		used += 8 + 4 + len(e.name) + pad(len(e.name)) + 8
	}
	if used > 200 || p.eof {
		t.Fatalf("%d bytes of directory information, eof %v", used, p.eof)
	}
}

// Only cookies and verifiers this export issued for this directory are
// accepted.
func TestReadDirPlusCookies(t *testing.T) {
	f := wideEngine(5)
	f.add("other", 5001, syscall.S_IFDIR|0o755, "")
	_, c := startServer(t, f)
	dir, _ := c.lookup(c.root(), "wide")
	other, _ := c.lookup(c.root(), "other")
	_, first := c.readDirPlus(dir, 0, 0, 1024, 1024)
	if first.eof || first.verf == 0 {
		t.Fatalf("first page %+v", first)
	}
	last := first.last()
	for name, tc := range map[string]struct{ cookie, verf uint64 }{
		"another verifier":               {last, first.verf ^ 1},
		"no verifier":                    {last, 0},
		"a cookie never issued":          {last + 1000, first.verf},
		"another directory's verifier":   {last, c.verifierOf(other)},
		"a dot cookie, another verifier": {cookieAfterDot, first.verf ^ 1},
	} {
		if s, _ := c.readDirPlus(dir, tc.cookie, tc.verf, 1024, 1024); s != uint32(nfsErrBadCook) {
			t.Errorf("%s: status %d", name, s)
		}
	}
	// Starting over keeps the directory's verifier.
	if _, again := c.readDirPlus(dir, 0, 0, 1024, 1024); again.verf != first.verf {
		t.Fatal("a fresh listing got another verifier")
	}
	_, rest := c.readDirPlus(dir, last, first.verf, 1024, 1<<20)
	if !rest.eof || !slices.Equal(append(first.names(), rest.names()...), wideNames(5)) {
		t.Fatalf("resumed listing %v %v", first.names(), rest.names())
	}
}

func (c *testClient) verifierOf(dir []byte) uint64 {
	c.t.Helper()
	_, p := c.readDirPlus(dir, 0, 0, 1024, 4096)
	return p.verf
}

// A page that cannot hold even its fixed part is TOOSMALL, at the end of the
// directory too.
func TestReadDirPlusTooSmall(t *testing.T) {
	_, c := startServer(t, wideEngine(5))
	dir, _ := c.lookup(c.root(), "wide")
	if s, _ := c.readDirPlus(dir, 0, 0, 1024, 200); s != uint32(nfsErrTooSmall) {
		t.Fatalf("no room for the dots: %d", s)
	}
	_, all := c.readDirPlus(dir, 0, 0, 1<<16, 1<<20)
	if !all.eof {
		t.Fatal("sanity")
	}
	if s, _ := c.readDirPlus(dir, all.last(), all.verf, 1024, 1); s != uint32(nfsErrTooSmall) {
		t.Fatalf("maxcount 1 at the end: %d", s)
	}
}

// One listing restarting from cookie 0 changes nothing for another listing
// of the same directory part-way through, even after a deletion.
func TestRestartDoesNotMoveCookies(t *testing.T) {
	f := wideEngine(10)
	_, c := startServer(t, f)
	dir, _ := c.lookup(c.root(), "wide")
	_, a := c.readDirPlus(dir, 0, 0, 1024, pageOverhead+3*184) // ".", "..", file-0000
	if names := a.names(); !slices.Equal(names, []string{".", "..", "file-0000"}) {
		t.Fatalf("first page %v", names)
	}
	f.remove("wide/file-0000")
	if got := c.readAll(dir, 0, 0, 1024); !slices.Equal(got, append([]string{".", ".."}, wideNames(10)[3:]...)) {
		t.Fatalf("the restarted listing %v", got)
	}
	if rest := c.readAll(dir, a.last(), a.verf, 1024); !slices.Equal(rest, wideNames(10)[3:]) {
		t.Fatalf("the paused listing resumed with %v", rest)
	}
}

// More paused listings than any fixed cache would hold all resume.
func TestManyPausedListingsResume(t *testing.T) {
	f := newFakeEngine()
	const dirs = 200
	for i := range dirs {
		f.add(fmt.Sprintf("d%03d", i), uint64(100+i), syscall.S_IFDIR|0o755, "")
		for j := range 4 {
			f.add(fmt.Sprintf("d%03d/f%d", i, j), uint64(1000+10*i+j), syscall.S_IFREG|0o644, "")
		}
	}
	_, c := startServer(t, f)
	root := c.root()
	handles := make([][]byte, dirs)
	paused := make([]listing, dirs)
	for i := range dirs {
		handles[i], _ = c.lookup(root, fmt.Sprintf("d%03d", i))
		_, paused[i] = c.readDirPlus(handles[i], 0, 0, 1024, pageOverhead+3*184)
	}
	for i := range dirs {
		rest := c.readAll(handles[i], paused[i].last(), paused[i].verf, 4096)
		if got := append(paused[i].names(), rest...); !slices.Equal(got, []string{".", "..", "f0", "f1", "f2", "f3"}) {
			t.Fatalf("d%03d listed %v", i, got)
		}
	}
}

// A directory of more than 1,000 entries read out of order: going back to
// an earlier cookie continues from its name.
func TestLargeDirectoryReadOutOfOrder(t *testing.T) {
	const n = 1200
	_, c := startServer(t, wideEngine(n))
	dir, _ := c.lookup(c.root(), "wide")
	var pages []listing
	cookie, verf := uint64(0), uint64(0)
	for {
		_, p := c.readDirPlus(dir, cookie, verf, 4096, 8192)
		pages = append(pages, p)
		if p.eof {
			break
		}
		cookie, verf = p.last(), p.verf
	}
	if len(pages) < 10 {
		t.Fatalf("%d pages", len(pages))
	}
	all := wideNames(n)
	for _, i := range []int{len(pages) - 2, 3, len(pages) / 2, 0} {
		from := pages[i].entries[len(pages[i].entries)-1]
		at := slices.Index(all, from.name)
		if rest := c.readAll(dir, from.cookie, verf, 8192); !slices.Equal(rest, all[at+1:]) {
			t.Fatalf("from %s: %d names, want %d", from.name, len(rest), len(all)-at-1)
		}
	}
}

// Changes between pages: a deletion skips no other name, and a name
// inserted after the position appears.
func TestChangesBetweenPages(t *testing.T) {
	f := wideEngine(10)
	_, c := startServer(t, f)
	dir, _ := c.lookup(c.root(), "wide")
	_, first := c.readDirPlus(dir, 0, 0, 1024, pageOverhead+6*184) // up to file-0003
	if first.entries[len(first.entries)-1].name != "file-0003" {
		t.Fatalf("first page %v", first.names())
	}
	f.remove("wide/file-0003") // the cookie's own name
	f.remove("wide/file-0001") // listed already
	f.remove("wide/file-0006") // not yet listed
	f.add("wide/file-0002a", 7001, syscall.S_IFREG|0o644, "")
	f.add("wide/file-0007a", 7002, syscall.S_IFREG|0o644, "")
	rest := c.readAll(dir, first.last(), first.verf, 1024)
	want := []string{"file-0004", "file-0005", "file-0007", "file-0007a", "file-0008", "file-0009"}
	if !slices.Equal(rest, want) {
		t.Fatalf("resumed with %v, want %v", rest, want)
	}
}

// "." and ".." are continuation states, not names: names sorting before
// ".." still follow them.
func TestContinuationAfterDots(t *testing.T) {
	f := newFakeEngine()
	f.add("d", 100, syscall.S_IFDIR|0o755, "")
	for i, name := range []string{"!a", "-a", "b"} {
		f.add("d/"+name, uint64(200+i), syscall.S_IFREG|0o644, "")
	}
	_, c := startServer(t, f)
	dir, _ := c.lookup(c.root(), "d")
	one, _ := entrySize(".", true)
	_, p := c.readDirPlus(dir, 0, 0, 1024, uint32(pageOverhead+one))
	if !slices.Equal(p.names(), []string{"."}) || p.last() != cookieAfterDot {
		t.Fatalf("first page %v", p.names())
	}
	_, p = c.readDirPlus(dir, cookieAfterDot, p.verf, 1024, uint32(pageOverhead+one))
	if !slices.Equal(p.names(), []string{".."}) || p.last() != cookieAfterDotDot {
		t.Fatalf("after .: %v", p.names())
	}
	if rest := c.readAll(dir, cookieAfterDotDot, p.verf, 4096); !slices.Equal(rest, []string{"!a", "-a", "b"}) {
		t.Fatalf("after ..: %v", rest)
	}
}

func TestReadDir(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	var names []string
	var cookie, verf uint64
	for {
		r := c.call(procReadDir, wire{}.opaque(root).u64(cookie).u64(verf).u32(256))
		if s := r.u32(); s != 0 {
			t.Fatalf("status %d", s)
		}
		r.postOpAttr()
		verf = r.u64()
		for r.u32() == 1 {
			fileid, name := r.u64(), string(r.opaque())
			cookie = r.u64()
			if fileid == 0 {
				t.Fatalf("%s has fileid 0", name)
			}
			names = append(names, name)
		}
		if r.u32() == 1 {
			break
		}
	}
	if want := []string{".", "..", "a.txt", "dir", "link", "secret"}; !slices.Equal(names, want) {
		t.Fatalf("listed %v, want %v", names, want)
	}
}

// Listing needs read permission on the directory.
func TestReadDirChecksPermission(t *testing.T) {
	f := sampleEngine()
	f.add("closed", 1100, syscall.S_IFDIR|0o300, "")
	_, c := startServer(t, f)
	dir, _ := c.lookup(c.root(), "closed")
	if s, _ := c.readDirPlus(dir, 0, 0, 1024, 4096); s != uint32(nfsErrAccess) {
		t.Fatalf("status %d", s)
	}
}

// READDIR looks nothing up: it takes no lookup references.
func TestReadDirTakesNoReferences(t *testing.T) {
	f := sampleEngine()
	_, c := startServer(t, f)
	root := c.root()
	c.call(procReadDir, wire{}.opaque(root).u64(0).u64(0).u32(4096))
	if n := f.refs("a.txt"); n != 0 {
		t.Fatalf("%d references", n)
	}
}

// The entry that did not fit gets no cookie.
func TestNoCookieForWhatDidNotFit(t *testing.T) {
	_, c := startServer(t, wideEngine(10))
	dir, _ := c.lookup(c.root(), "wide")
	_, p := c.readDirPlus(dir, 0, 0, 1024, pageOverhead+3*184)
	if s, _ := c.readDirPlus(dir, p.last()+1, p.verf, 1024, 1024); s != uint32(nfsErrBadCook) {
		t.Fatalf("a cookie never sent: %d", s)
	}
}
