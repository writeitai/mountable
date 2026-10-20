package nfs

import (
	"encoding/binary"
	"sync"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
)

// Directory cookies continue by name, not by position (design §4,
// "Directories"). A cookie stands for the last name a page returned, and
// the next page lists the names after it in the filer's sorted order, so a
// deletion never skips another name and a name inserted after the position
// simply appears. "." and ".." are not names in that order: their cookies
// are reserved continuation states.
const (
	cookieAfterDot    = 1 // ".." comes next
	cookieAfterDotDot = 2 // the names start from the beginning
	firstNameCookie   = 3
)

// cookieTable maps one directory's issued cookies to the names they stand
// for. Tables are kept for the life of the export and never evicted: a
// cookie the export forgot would have to be answered NFS3ERR_BAD_COOKIE,
// which the macOS client takes for the end of the directory. They grow with
// the names listed, like the retained lookup references.
type cookieTable struct {
	mu     sync.Mutex
	names  []string // by cookie - firstNameCookie
	byName map[string]uint64
}

func (t *cookieTable) cookie(name string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c, ok := t.byName[name]; ok {
		return c
	}
	c := firstNameCookie + uint64(len(t.names))
	t.names = append(t.names, name)
	t.byName[name] = c
	return c
}

func (t *cookieTable) name(cookie uint64) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if cookie < firstNameCookie || cookie-firstNameCookie >= uint64(len(t.names)) {
		return "", false
	}
	return t.names[cookie-firstNameCookie], true
}

// directories holds the cookie tables, by directory identity.
type directories struct {
	mu     sync.Mutex
	tables map[object]*cookieTable
}

func (d *directories) table(dir object) *cookieTable {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := object{inode: dir.inode, incarnation: dir.incarnation}
	t := d.tables[key]
	if t == nil {
		t = &cookieTable{byName: map[string]uint64{}}
		d.tables[key] = t
	}
	return t
}

// verifier is a directory's cookie verifier: constant for the life of the
// export (RFC 1813 §3.3.16 allows that while cookies stay valid), a keyed
// MAC of the directory's identity, never zero, so a new export never
// accepts an old one.
func (s *service) verifier(dir object) uint64 {
	b := binary.BigEndian.AppendUint64([]byte("cookieverf"), dir.inode)
	b = binary.BigEndian.AppendUint64(b, dir.incarnation)
	v := binary.BigEndian.Uint64(s.handles.mac(b))
	if v == 0 {
		v = 1
	}
	return v
}

// listed is one entry a page carries.
type listed struct {
	name   string
	cookie uint64
	ino    uint64 // the fileid, if no handle is issued for it
	obj    *object
	entry  *weedmount.ListedEntry // as the engine listed it; nil for "." and ".."
}

func (s *service) readDir(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	cookie, verf := d.uint64(), d.uint64()
	count := d.uint32()
	if !d.done() {
		return garbage()
	}
	return s.list(o, fh, cookie, verf, int(count), int(count), false)
}

func (s *service) readDirPlus(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	cookie, verf := d.uint64(), d.uint64()
	dirCount, maxCount := d.uint32(), d.uint32()
	if !d.done() {
		return garbage()
	}
	return s.list(o, fh, cookie, verf, int(dirCount), int(maxCount), true)
}

// pageOverhead is a listing reply without entries: status, the directory's
// post_op_attr, the verifier, the end of the list and eof.
const pageOverhead = 4 + 4 + fattrSize + 8 + 4 + 4

// entrySize is an entry's encoded size in the reply, and its share of
// READDIRPLUS's dircount (fileid, name and cookie).
func entrySize(name string, plus bool) (size, dirSize int) {
	dirSize = 8 + 4 + len(name) + pad(len(name)) + 8
	size = 4 + dirSize // value_follows
	if plus {
		size += 4 + fattrSize + 4 + 4 + handleSize // post_op_attr, post_op_fh3
	}
	return size, dirSize
}

// page collects one page within the reply's two budgets: bytes, the space
// left for entries in the whole reply (maxcount), and dirBytes, what
// READDIRPLUS's dircount allows for names, fileids and cookies alone.
type page struct {
	plus            bool
	bytes, dirBytes int
	entries         []listed
	full            bool
}

// fits reports whether an entry of that name still fits; once one does
// not, the page is full.
func (p *page) fits(name string) bool {
	size, dirSize := entrySize(name, p.plus)
	if p.full || size > p.bytes || p.plus && dirSize > p.dirBytes {
		p.full = true
	}
	return !p.full
}

// add takes an entry if it fits; its cookie is made only then, so no
// cookie is issued for an entry the page does not carry.
func (p *page) add(entry listed, cookie func() uint64) bool {
	if !p.fits(entry.name) {
		return false
	}
	size, dirSize := entrySize(entry.name, p.plus)
	p.bytes -= size
	p.dirBytes -= dirSize
	entry.cookie = cookie()
	p.entries = append(p.entries, entry)
	return true
}

// list answers READDIR and READDIRPLUS: one page from cookie, within both
// budgets.
func (s *service) list(o *op, fh []byte, cookie, verf uint64, dirCount, maxCount int, plus bool) reply {
	dir, status := s.resolve(o, fh)
	if status != nfsOK {
		return s.failure(o.call, status)
	}
	fail := func(status nfsStatus) reply {
		e := &encoder{}
		e.uint32(uint32(status))
		putPostOpAttr(e, &dir.attr, s.fsid)
		return reply{body: e.buf}
	}
	switch {
	case !isDir(&dir.attr):
		return fail(nfsErrNotDir)
	case !allowed(o.cred, &dir.attr, permRead):
		return fail(nfsErrAccess)
	case maxCount < pageOverhead:
		return fail(nfsErrTooSmall) // not even an empty page fits
	}
	table := s.dirs.table(dir)
	var after string // list the names after this one
	dots := []string{".", ".."}
	switch {
	case cookie == 0:
	case verf != s.verifier(dir):
		return fail(nfsErrBadCook)
	case cookie == cookieAfterDot:
		dots = dots[1:]
	case cookie == cookieAfterDotDot:
		dots = nil
	default:
		name, ok := table.name(cookie)
		if !ok {
			return fail(nfsErrBadCook)
		}
		after, dots = name, nil
	}

	p := &page{plus: plus, bytes: min(maxCount, maxTransfer) - pageOverhead, dirBytes: dirCount}
	for i, dot := range dots {
		cookie := firstNameCookie - uint64(len(dots)) + uint64(i)
		p.add(listed{name: dot}, func() uint64 { return cookie })
	}
	eof := false
	if !p.full {
		var st nfsStatus
		if eof, st = s.names(dir, after, p, table); st != nfsOK {
			return fail(st)
		}
	}
	// The names were listed by the directory's path: answer, and issue
	// handles for them, only if that is still this directory.
	if st := s.still(o, dir); st != nfsOK {
		s.drop(p.entries)
		if st == nfsErrStale {
			return s.failure(o.call, st)
		}
		return fail(st)
	}
	if len(p.entries) == 0 && !eof {
		return fail(nfsErrTooSmall)
	}
	for i := range p.entries {
		s.fill(o, dir, &p.entries[i], plus)
	}

	e := &encoder{}
	e.uint32(uint32(nfsOK))
	putPostOpAttr(e, &dir.attr, s.fsid)
	e.uint64(s.verifier(dir))
	for _, entry := range p.entries {
		e.bool(true)
		e.uint64(entry.ino)
		e.string(entry.name)
		e.uint64(entry.cookie)
		if plus {
			if entry.obj == nil {
				e.bool(false)
				e.bool(false)
				continue
			}
			putPostOpAttr(e, &entry.obj.attr, s.fsid)
			e.bool(true)
			e.opaque(s.fh(*entry.obj))
		}
	}
	e.bool(false)
	e.bool(eof)
	return reply{body: e.buf}
}

// names adds the directory's names after the given one, in the store's
// order, until the page is full or the directory ends. They come from the
// engine's own listing, the source its lookups use, by name; on a
// READDIRPLUS page with their attributes, each holding a lookup reference.
func (s *service) names(dir object, after string, p *page, table *cookieTable) (eof bool, status nfsStatus) {
	eof, st := s.engine.ReadDirectoryAfter(dir.inode, after, p.plus, p.fits, func(e *weedmount.ListedEntry) bool {
		return p.add(listed{name: e.Name, ino: e.Inode, entry: e}, func() uint64 { return table.cookie(e.Name) })
	})
	if st != fuse.OK {
		s.drop(p.entries)
		return false, statusOf(st)
	}
	return eof, nfsOK
}

// drop gives back the lookup references a page not sent took.
func (s *service) drop(entries []listed) {
	for _, entry := range entries {
		if entry.entry != nil && entry.entry.Out != nil {
			s.engine.Forget(entry.entry.Inode, 1)
		}
	}
}

// fill gives an entry its fileid and, on a READDIRPLUS page, its handle:
// "." is the directory, ".." its parent, and a listed name the object the
// listing referenced, whose attributes it carries.
func (s *service) fill(o *op, dir object, entry *listed, plus bool) {
	var obj object
	status := nfsOK
	switch {
	case entry.name == ".":
		obj = dir
	case entry.name == "..":
		obj, status = s.parent(o, dir)
	case !plus:
		return // the listing's fileid
	default:
		obj = s.listedObject(entry.entry)
		// As LOOKUP does: a later miss of the name then checks this object.
		s.rebind(name{dir: dir.inode, name: entry.name}, obj.inode)
	}
	if status != nfsOK {
		entry.ino = dir.inode // a fileid of zero would hide the entry
		return
	}
	entry.ino, entry.obj = obj.inode, &obj
}

// listedObject issues the handle for an entry a READDIRPLUS listing
// referenced, as found does for a lookup, from the attributes the listing
// carries: the store's inode is the identity.
func (s *service) listedObject(e *weedmount.ListedEntry) object {
	attr := e.Out.Attr
	attr.Ino = e.StoredInode
	if attr.Ino == 0 { // as GetAttr reports an entry without one
		attr.Ino = e.Inode
	}
	incarnation := s.objects.issue(e.Inode, &attr)
	attr.Ino = e.Inode
	return object{inode: e.Inode, incarnation: incarnation, attr: attr}
}
