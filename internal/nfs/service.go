package nfs

import (
	"hash/fnv"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
)

// Config is one export: the engine it serves and the one user it serves it
// to.
type Config struct {
	Engine Engine
	// UID is the mounting user's. Calls from any other user but root are
	// refused: on the private socket, the UID comes from the kernel client
	// acting for the real caller.
	UID uint32
	// ReadOnly makes every procedure that would change something answer
	// NFS3ERR_ROFS, for a read-only session.
	ReadOnly bool
}

// NewServer returns a server for one export.
func NewServer(c Config) (*Server, error) {
	h, err := newHandles()
	if err != nil {
		return nil, err
	}
	fsid := fnv.New64a()
	fsid.Write([]byte(c.Engine.MountRoot()))
	s := &service{
		engine:    c.Engine,
		uid:       c.UID,
		readOnly:  c.ReadOnly,
		handles:   h,
		exclusive: &exclusiveSet{inodes: map[uint64]struct{}{}},
		objects:   newObjects(c.Engine),
		dirs:      &directories{tables: map[object]*cookieTable{}},
		fsid:      fsid.Sum64() | 1, // never zero
		workers:   make(chan struct{}, maxWorkers),
		ops:       map[*op]struct{}{},
		endCh:     make(chan struct{}),
	}
	if s.writes, err = newWrites(s); err != nil {
		return nil, err
	}
	return &Server{
		svc:     s,
		pending: make(chan struct{}, maxPending),
		waiting: make(chan struct{}, maxPending),
		drc:     newReplyCache(),
		conns:   map[net.Conn]struct{}{},
	}, nil
}

// Flush persists every write accepted and not yet committed, and releases
// the write handles, as a clean stop does once the mount is gone.
func (s *Server) Flush() { s.svc.writes.flushAll() }

// End makes every call fail at once, before the mount is forced off: the
// session ended, and nothing may be served from it any more. Calls being
// worked on are cancelled and answer NFS3ERR_IO, whatever the engine
// returns.
func (s *Server) End() { s.svc.end() }

// maxWorkers bounds the calls the engine works on at once, so a gateway
// that stops answering holds up a bounded number of them; the others are
// answered "retry later".
const maxWorkers = 64

// Each call's time limits (design §4, "bounded replies"). The client is
// mounted with a fixed 30-second timeout (timeo=300 with dumbtimer), so a
// call answered within rpcDeadline never makes a live server look dead.
var (
	// rpcDeadline is each call's end-to-end deadline.
	rpcDeadline = 25 * time.Second
	// prepareBudget is how much of it a call may take before it changes
	// anything: waiting for a worker, locks and admission. A call that has
	// not started changing anything by then answers NFS3ERR_JUKEBOX ("retry
	// later") and changes nothing; the client's retry starts afresh.
	prepareBudget = 10 * time.Second
)

type service struct {
	engine    Engine
	uid       uint32
	readOnly  bool
	writes    *writes
	exclusive *exclusiveSet
	handles   *handles
	objects   *objects
	dirs      *directories
	fsid      uint64
	workers   chan struct{}
	ended     atomic.Bool
	endCh     chan struct{} // closed by end
	endOnce   sync.Once

	mu   sync.Mutex
	ops  map[*op]struct{} // the calls being worked on
	work sync.WaitGroup   // their work, including what outlives its answer
}

// immediate answers a call that never reaches the engine: MOUNT, NULL, an
// unknown procedure. ok is false for any other call.
func (s *service) immediate(c *call) (reply, bool) {
	switch _, known := procedures[c.proc]; {
	case c.prog == programMount:
		return s.mount(c), true
	case !known:
		return reply{stat: acceptProcUnavail}, true
	case c.proc == procNull && len(c.args) != 0:
		return garbage(), true
	case c.proc == procNull:
		return reply{}, true
	}
	return reply{}, false
}

// admit refuses an NFS call that may not be served at all: without
// AUTH_SYS, from another user, or after End. It runs before the call is
// queued or looked up in the duplicate-request cache.
func (s *service) admit(c *call) (reply, bool) {
	switch {
	case c.cred.flavor != authSys:
		return reply{denied: true, stat: rejectAuthError, body: be32(authTooWeak)}, false
	case c.cred.uid != s.uid && c.cred.uid != 0:
		return s.failure(c, nfsErrAccess), false
	case s.ended.Load():
		return s.failure(c, nfsErrIO), false
	}
	return reply{}, true
}

// handle works on an admitted NFS call. late, when not nil, delivers the
// call's own reply after an answer of NFS3ERR_IO given while it was still
// changing things.
func (s *service) handle(c *call) (r reply, late <-chan reply) {
	proc := procedures[c.proc]
	fn := proc.fn
	if s.readOnly && !proc.changesNothing {
		fn = readOnlyExport
	}
	return s.run(c, proc.changesNothing || proc.flush, func(o *op) reply { return fn(s, o) })
}

// end marks the export ended and cancels the calls being worked on.
func (s *service) end() {
	s.ended.Store(true)
	s.endOnce.Do(func() { close(s.endCh) })
	s.mu.Lock()
	defer s.mu.Unlock()
	for o := range s.ops {
		// A call still preparing is abandoned first, so it can no longer
		// begin changing things once it sees the cancellation.
		o.state.CompareAndSwap(opPreparing, opAbandoned)
		o.stop()
	}
}

// wait waits, at most d, for the work still running, as Close does once
// the connections are gone.
func (s *service) wait(d time.Duration) {
	done := make(chan struct{})
	go func() {
		s.work.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// procedure is one NFSv3 procedure. One that changesNothing may answer
// "retry later" at any point before the call's deadline, and is not kept in
// the duplicate-request cache. One that is a flush (WRITE and COMMIT,
// which only add to or persist what clients accepted) may answer "retry
// later" at any point too, but is kept in the cache.
type procedure struct {
	fn             func(s *service, o *op) reply
	changesNothing bool
	flush          bool
}

var procedures = map[uint32]procedure{
	procNull:        {changesNothing: true},
	procGetAttr:     {fn: (*service).getAttr, changesNothing: true},
	procSetAttr:     {fn: (*service).setAttr},
	procLookup:      {fn: (*service).lookup, changesNothing: true},
	procAccess:      {fn: (*service).access, changesNothing: true},
	procReadlink:    {fn: (*service).readlink, changesNothing: true},
	procRead:        {fn: (*service).read, changesNothing: true},
	procWrite:       {fn: (*service).write, flush: true},
	procCreate:      {fn: (*service).create},
	procMkdir:       {fn: (*service).mkdir},
	procSymlink:     {fn: (*service).symlink},
	procMknod:       {fn: (*service).mknod},
	procRemove:      {fn: (*service).remove},
	procRmdir:       {fn: (*service).rmdir},
	procRename:      {fn: (*service).rename},
	procLink:        {fn: (*service).link},
	procReadDir:     {fn: (*service).readDir, changesNothing: true},
	procReadDirPlus: {fn: (*service).readDirPlus, changesNothing: true},
	procFSStat:      {fn: (*service).fsStat, changesNothing: true},
	procFSInfo:      {fn: (*service).fsInfo, changesNothing: true},
	procPathConf:    {fn: (*service).pathConf, changesNothing: true},
	procCommit:      {fn: (*service).commit, flush: true},
}

// failWords is, per procedure, the size in XDR words of its failure result
// after the status: its post_op_attr and wcc_data, all absent.
var failWords = [...]int{
	procSetAttr: 2, procLookup: 1, procAccess: 1, procReadlink: 1, procRead: 1,
	procWrite: 2, procCreate: 2, procMkdir: 2, procSymlink: 2, procMknod: 2,
	procRemove: 2, procRmdir: 2, procRename: 4, procLink: 3, procReadDir: 1,
	procReadDirPlus: 1, procFSStat: 1, procFSInfo: 1, procPathConf: 1, procCommit: 2,
}

// nonIdempotent procedures are the ones the duplicate-request cache
// answers for: applying one of them twice is not the same as once.
func nonIdempotent(proc uint32) bool {
	p, ok := procedures[proc]
	return ok && !p.changesNothing
}

// readOnlyExport answers every procedure that would change something.
func readOnlyExport(s *service, o *op) reply { return s.failure(o.call, nfsErrROFS) }

// failure is an NFS result with an error status and no attributes.
func (s *service) failure(c *call, status nfsStatus) reply {
	e := &encoder{}
	e.uint32(uint32(status))
	for range failWords[c.proc] {
		e.bool(false)
	}
	return reply{body: e.buf, retry: status == nfsErrJukebox}
}

// retryLater answers NFS3ERR_JUKEBOX: the call changed nothing, and the
// client tries it again later.
func (s *service) retryLater(c *call) reply { return s.failure(c, nfsErrJukebox) }

// garbage answers a call whose arguments do not decode.
func garbage() reply { return reply{stat: acceptGarbageArgs} }

// op is one NFS call while the engine works on it.
type op struct {
	*call
	// cancel is closed once the call was answered without waiting for the
	// work, or the export ended: the engine's cancellable waits give up.
	cancel chan struct{}
	once   sync.Once
	state  atomic.Int32
}

func (o *op) stop() { o.once.Do(func() { close(o.cancel) }) }

const (
	opPreparing int32 = iota
	opMutating
	opAbandoned
)

// beginMutation is the point from which the call may change what clients
// see; until then it may still be answered "retry later". It reports false
// once the call was already answered that way: the procedure must then
// stop without changing anything.
func (o *op) beginMutation() bool { return o.state.CompareAndSwap(opPreparing, opMutating) }

// run works on a call with a bounded wait: a worker within prepareBudget,
// then the procedure. A call still preparing at its limit (prepareBudget,
// or rpcDeadline for a procedure that changes nothing) answers "retry later"; one
// already changing things gets until rpcDeadline, and then answers
// NFS3ERR_IO, since its outcome is unknown; late then delivers the reply
// the work makes, for the duplicate-request cache. Work that outlives its
// answer finishes in the background, keeping its worker until it does.
func (s *service) run(c *call, changesNothing bool, fn func(o *op) reply) (r reply, late <-chan reply) {
	wait := time.NewTimer(time.Until(c.received.Add(prepareBudget)))
	select {
	case s.workers <- struct{}{}:
		wait.Stop()
	case <-wait.C:
		return s.retryLater(c), nil
	case <-s.endCh:
		return s.failure(c, nfsErrIO), nil
	}
	o := &op{call: c, cancel: make(chan struct{})}
	s.mu.Lock()
	if s.ended.Load() { // ended while the call waited for its worker
		s.mu.Unlock()
		<-s.workers
		return s.failure(c, nfsErrIO), nil
	}
	s.ops[o] = struct{}{}
	s.work.Add(1)
	s.mu.Unlock()
	result := make(chan reply, 1)
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.ops, o)
			s.mu.Unlock()
			<-s.workers
			s.work.Done()
		}()
		result <- fn(o)
	}()
	limit := c.received.Add(prepareBudget)
	if changesNothing {
		limit = c.received.Add(rpcDeadline)
	}
	timer := time.NewTimer(time.Until(limit))
	defer timer.Stop()
	select {
	case r := <-result:
		return s.unlessEnded(c, r), nil
	case <-s.endCh:
		return s.failure(c, nfsErrIO), nil
	case <-timer.C:
	}
	if o.state.CompareAndSwap(opPreparing, opAbandoned) {
		o.stop()
		return s.retryLater(c), nil
	}
	timer.Reset(time.Until(c.received.Add(rpcDeadline)))
	select {
	case r := <-result:
		return s.unlessEnded(c, r), nil
	case <-s.endCh:
		return s.failure(c, nfsErrIO), nil
	case <-timer.C:
		o.stop()
		return s.failure(c, nfsErrIO), result
	}
}

// unlessEnded drops a result the engine produced after the export ended.
func (s *service) unlessEnded(c *call, r reply) reply {
	if s.ended.Load() {
		return s.failure(c, nfsErrIO)
	}
	return r
}

// header is the request header WFS operations take, acting for the caller.
func header(inode uint64, cred credential) fuse.InHeader {
	return fuse.InHeader{NodeId: inode, Caller: fuse.Caller{Owner: fuse.Owner{Uid: cred.uid, Gid: cred.gid}}}
}
