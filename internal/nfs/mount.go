package nfs

// The MOUNT v3 program (RFC 1813 Appendix I): the root handle of the one
// export, "/", and nothing else. DUMP and EXPORT list nothing; UMNT is
// advisory, and the mount's end is read from the OS mount table instead.

func (s *service) mount(c *call) reply {
	switch c.proc {
	case mountProcNull:
		if len(c.args) != 0 {
			return garbage()
		}
		return reply{}
	case mountProcMnt, mountProcDump, mountProcUmnt, mountProcUmntAll, mountProcExport:
	default:
		return reply{stat: acceptProcUnavail}
	}
	if c.cred.flavor != authSys {
		return reply{denied: true, stat: rejectAuthError, body: be32(authTooWeak)}
	}
	d := &decoder{buf: c.args}
	var path string
	if c.proc == mountProcMnt || c.proc == mountProcUmnt {
		path = d.string(mountPathLen)
	}
	if !d.done() {
		return garbage()
	}
	e := &encoder{}
	switch c.proc {
	case mountProcMnt:
		switch {
		case c.cred.uid != s.uid && c.cred.uid != 0:
			e.uint32(mountErrAccess)
		case s.ended.Load():
			e.uint32(mountErrServFail)
		case path != "/":
			e.uint32(mountErrNoEnt)
		default:
			e.uint32(mountOK)
			e.opaque(s.fh(object{inode: rootInode, incarnation: rootIncarnation}))
			e.uint32(1) // one flavor: AUTH_SYS
			e.uint32(authSys)
		}
	case mountProcDump, mountProcExport:
		e.bool(false) // an empty list
	}
	return reply{body: e.buf}
}
