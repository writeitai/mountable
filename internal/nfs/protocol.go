// Derived from go-nfs (github.com/willscott/go-nfs at
// 6274d051a57fe87e8cb9cda60e38824db1ab08a7: nfsinterface.go,
// mountinterface.go, conn.go and errors.go), Apache License 2.0; see LICENSE
// and NOTICE in this directory. Changed: only the NFSv3 and MOUNT v3
// constants this server uses are kept, the RPC message constants come from
// RFC 5531, and the names follow the RFCs.

package nfs

// The two programs served, each at version 3 only (RFC 1813).
const (
	programNFS   = 100003
	programMount = 100005
	version3     = 3
)

// NFSv3 procedures (RFC 1813 §3.3).
const (
	procNull        = 0
	procGetAttr     = 1
	procSetAttr     = 2
	procLookup      = 3
	procAccess      = 4
	procReadlink    = 5
	procRead        = 6
	procWrite       = 7
	procCreate      = 8
	procMkdir       = 9
	procSymlink     = 10
	procMknod       = 11
	procRemove      = 12
	procRmdir       = 13
	procRename      = 14
	procLink        = 15
	procReadDir     = 16
	procReadDirPlus = 17
	procFSStat      = 18
	procFSInfo      = 19
	procPathConf    = 20
	procCommit      = 21
)

// nfsstat3 (RFC 1813 §2.6).
type nfsStatus uint32

const (
	nfsOK          nfsStatus = 0
	nfsErrPerm     nfsStatus = 1
	nfsErrNoEnt    nfsStatus = 2
	nfsErrIO       nfsStatus = 5
	nfsErrAccess   nfsStatus = 13
	nfsErrExist    nfsStatus = 17
	nfsErrXDev     nfsStatus = 18
	nfsErrNotDir   nfsStatus = 20
	nfsErrIsDir    nfsStatus = 21
	nfsErrInval    nfsStatus = 22
	nfsErrFBig     nfsStatus = 27
	nfsErrNoSpc    nfsStatus = 28
	nfsErrROFS     nfsStatus = 30
	nfsErrMLink    nfsStatus = 31
	nfsErrNameLong nfsStatus = 63
	nfsErrNotEmpty nfsStatus = 66
	nfsErrDQuot    nfsStatus = 69
	nfsErrStale    nfsStatus = 70
	nfsErrBadFH    nfsStatus = 10001
	nfsErrNotSync  nfsStatus = 10002
	nfsErrBadCook  nfsStatus = 10003
	nfsErrNotSupp  nfsStatus = 10004
	nfsErrTooSmall nfsStatus = 10005
	nfsErrBadType  nfsStatus = 10007
	nfsErrJukebox  nfsStatus = 10008
)

// ftype3 (RFC 1813 §2.6).
const (
	typeReg  = 1
	typeDir  = 2
	typeBlk  = 3
	typeChr  = 4
	typeLnk  = 5
	typeSock = 6
	typeFifo = 7
)

// ACCESS3 bits (RFC 1813 §3.3.4).
const (
	accessRead    = 0x01
	accessLookup  = 0x02
	accessModify  = 0x04
	accessExtend  = 0x08
	accessDelete  = 0x10
	accessExecute = 0x20
)

// FSINFO3 properties (RFC 1813 §3.3.19).
const (
	fsfLink        = 0x01
	fsfSymlink     = 0x02
	fsfHomogeneous = 0x08
	fsfCanSetTime  = 0x10
)

// MOUNT v3 procedures and mountstat3 (RFC 1813 Appendix I).
const (
	mountProcNull    = 0
	mountProcMnt     = 1
	mountProcDump    = 2
	mountProcUmnt    = 3
	mountProcUmntAll = 4
	mountProcExport  = 5

	mountOK          = 0
	mountErrNoEnt    = 2
	mountErrAccess   = 13
	mountErrServFail = 10006

	mountPathLen = 1024
)

// ONC RPC message fields (RFC 5531 §9).
const (
	rpcVersion = 2

	msgCall  = 0
	msgReply = 1

	replyAccepted = 0
	replyDenied   = 1

	acceptSuccess      = 0
	acceptProgUnavail  = 1
	acceptProgMismatch = 2
	acceptProcUnavail  = 3
	acceptGarbageArgs  = 4
	acceptSystemErr    = 5

	rejectRPCMismatch = 0
	rejectAuthError   = 1

	authNone = 0
	authSys  = 1

	authBadCred = 1
	authTooWeak = 5

	// maxAuthBytes is the largest opaque_auth body (RFC 5531 §8.2).
	maxAuthBytes = 400
)
