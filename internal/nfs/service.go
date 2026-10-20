package nfs

import (
	"hash/fnv"
	"net"
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
}

// NewServer returns a server for one read-only export. The write path is
// not implemented yet: every procedure that would change something answers
// NFS3ERR_ROFS, and the export must be mounted read-only.
func NewServer(c Config) (*Server, error) {
	h, err := newHandles()
	if err != nil {
		return nil, err
	}
	fsid := fnv.New64a()
	fsid.Write([]byte(c.Engine.MountRoot()))
	s := &service{
		engine:  c.Engine,
		uid:     c.UID,
		handles: h,
		objects: newObjects(c.Engine),
		dirs:    newEnumerations(c.Engine),
		fsid:    fsid.Sum64() | 1, // never zero
		workers: make(chan struct{}, maxWorkers),
	}
	return &Server{
		svc:     s,
		pending: make(chan struct{}, maxPending),
		drc:     newReplyCache(),
		conns:   map[net.Conn]struct{}{},
	}, nil
}

// End makes every further call fail at once, before the mount is forced
// off: the session ended, and nothing may be served from it any more.
func (s *Server) End() { s.svc.ended.Store(true) }

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
	engine  Engine
	uid     uint32
	handles *handles
	objects *objects
	dirs    *enumerations
	fsid    uint64
	workers chan struct{}
	ended   atomic.Bool
}

// handle answers one NFS or MOUNT v3 call.
func (s *service) handle(c *call) reply {
	if c.prog == programMount {
		return s.mount(c)
	}
	proc, known := procedures[c.proc]
	if !known {
		return reply{stat: acceptProcUnavail}
	}
	if c.proc == procNull {
		return reply{}
	}
	if c.cred.flavor != authSys {
		return reply{denied: true, stat: rejectAuthError, body: be32(authTooWeak)}
	}
	if c.cred.uid != s.uid && c.cred.uid != 0 {
		return s.failure(c, nfsErrAccess)
	}
	if s.ended.Load() {
		return s.failure(c, nfsErrIO)
	}
	return s.run(c, proc.changesNothing, func(o *op) reply { return proc.fn(s, o) })
}

// procedure is one NFSv3 procedure. One that changesNothing may answer
// "retry later" at any point before the call's deadline, and is not kept in
// the duplicate-request cache.
type procedure struct {
	fn             func(s *service, o *op) reply
	changesNothing bool
}

var procedures = map[uint32]procedure{
	procNull:        {changesNothing: true},
	procGetAttr:     {fn: (*service).getAttr, changesNothing: true},
	procSetAttr:     {fn: readOnlyExport},
	procLookup:      {fn: (*service).lookup, changesNothing: true},
	procAccess:      {fn: (*service).access, changesNothing: true},
	procReadlink:    {fn: (*service).readlink, changesNothing: true},
	procRead:        {fn: (*service).read, changesNothing: true},
	procWrite:       {fn: readOnlyExport},
	procCreate:      {fn: readOnlyExport},
	procMkdir:       {fn: readOnlyExport},
	procSymlink:     {fn: readOnlyExport},
	procMknod:       {fn: readOnlyExport},
	procRemove:      {fn: readOnlyExport},
	procRmdir:       {fn: readOnlyExport},
	procRename:      {fn: readOnlyExport},
	procLink:        {fn: readOnlyExport},
	procReadDir:     {fn: (*service).readDir, changesNothing: true},
	procReadDirPlus: {fn: (*service).readDirPlus, changesNothing: true},
	procFSStat:      {fn: (*service).fsStat, changesNothing: true},
	procFSInfo:      {fn: (*service).fsInfo, changesNothing: true},
	procPathConf:    {fn: (*service).pathConf, changesNothing: true},
	procCommit:      {fn: readOnlyExport},
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
	// work: the engine's cancellable waits give up.
	cancel chan struct{}
	state  atomic.Int32
}

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
// NFS3ERR_IO, since its outcome is unknown. Work that outlives its answer
// finishes in the background, keeping its worker until it does.
func (s *service) run(c *call, changesNothing bool, fn func(o *op) reply) reply {
	wait := time.NewTimer(time.Until(c.received.Add(prepareBudget)))
	select {
	case s.workers <- struct{}{}:
		wait.Stop()
	case <-wait.C:
		return s.retryLater(c)
	}
	o := &op{call: c, cancel: make(chan struct{})}
	result := make(chan reply, 1)
	go func() {
		defer func() { <-s.workers }()
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
		return r
	case <-timer.C:
	}
	if o.state.CompareAndSwap(opPreparing, opAbandoned) {
		close(o.cancel)
		return s.retryLater(c)
	}
	timer.Reset(time.Until(c.received.Add(rpcDeadline)))
	select {
	case r := <-result:
		return r
	case <-timer.C:
		close(o.cancel)
		return s.failure(c, nfsErrIO)
	}
}

// header is the request header WFS operations take, acting for the caller.
func header(inode uint64, cred credential) fuse.InHeader {
	return fuse.InHeader{NodeId: inode, Caller: fuse.Caller{Owner: fuse.Owner{Uid: cred.uid, Gid: cred.gid}}}
}
