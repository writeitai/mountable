package nfs

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
)

// handleSize is the length of every file handle, the root's included: the
// macOS client sizes directory buffers from the parent's handle length.
const handleSize = 48

// handleTag is the format tag the handles start with.
var handleTag = [4]byte{'M', 'T', 'B', 1}

// handles makes and checks file handles: format tag, process epoch, WFS
// inode, incarnation, and a MAC under a key that lives only in this
// process. 4+8+8+8+20 = 48 bytes.
type handles struct {
	key   [32]byte
	epoch [8]byte
}

func newHandles() (*handles, error) {
	h := &handles{}
	if _, err := rand.Read(h.key[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(h.epoch[:]); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *handles) make(inode, incarnation uint64) []byte {
	b := make([]byte, 0, handleSize)
	b = append(b, handleTag[:]...)
	b = append(b, h.epoch[:]...)
	b = binary.BigEndian.AppendUint64(b, inode)
	b = binary.BigEndian.AppendUint64(b, incarnation)
	return append(b, h.mac(b)...)
}

// parse returns the inode and incarnation a handle names. A handle of
// another length or format is BADHANDLE; one with a bad MAC or from another
// process lifetime is STALE.
func (h *handles) parse(fh []byte) (inode, incarnation uint64, status nfsStatus) {
	if len(fh) != handleSize || [4]byte(fh[:4]) != handleTag {
		return 0, 0, nfsErrBadFH
	}
	if !hmac.Equal(fh[28:], h.mac(fh[:28])) || [8]byte(fh[4:12]) != h.epoch {
		return 0, 0, nfsErrStale
	}
	return binary.BigEndian.Uint64(fh[12:20]), binary.BigEndian.Uint64(fh[20:28]), nfsOK
}

func (h *handles) mac(b []byte) []byte {
	m := hmac.New(sha256.New, h.key[:])
	m.Write(b)
	return m.Sum(nil)[:20]
}
