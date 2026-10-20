package nfs

import (
	"encoding/binary"
	"errors"
)

// errGarbage is a request whose arguments do not decode.
var errGarbage = errors.New("malformed XDR")

// decoder reads XDR (RFC 4506) from one request's arguments. Every length is
// checked against what is left before it is used, so a hostile length never
// allocates anything; the first error sticks.
type decoder struct {
	buf []byte
	err error
}

func (d *decoder) take(n int) []byte {
	if d.err != nil {
		return nil
	}
	if n < 0 || n > len(d.buf) {
		d.err = errGarbage
		return nil
	}
	b := d.buf[:n]
	d.buf = d.buf[n:]
	return b
}

func (d *decoder) uint32() uint32 {
	b := d.take(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (d *decoder) uint64() uint64 {
	b := d.take(8)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func (d *decoder) bool() bool {
	switch d.uint32() {
	case 0:
		return false
	case 1:
		return true
	}
	d.err = errGarbage
	return false
}

// opaque reads variable-length opaque data of at most max bytes. The result
// points into the request.
func (d *decoder) opaque(max int) []byte {
	n := d.uint32()
	if d.err != nil {
		return nil
	}
	if n > uint32(max) {
		d.err = errGarbage
		return nil
	}
	b := d.take(int(n))
	d.take(pad(int(n)))
	return b
}

func (d *decoder) string(max int) string { return string(d.opaque(max)) }

// done reports whether the arguments decoded with nothing left over.
func (d *decoder) done() bool { return d.err == nil && len(d.buf) == 0 }

// pad is the number of zero bytes that bring n up to a multiple of four.
func pad(n int) int { return (4 - n%4) % 4 }

// encoder writes XDR.
type encoder struct {
	buf []byte
}

func (e *encoder) uint32(v uint32) { e.buf = binary.BigEndian.AppendUint32(e.buf, v) }

func (e *encoder) uint64(v uint64) { e.buf = binary.BigEndian.AppendUint64(e.buf, v) }

func (e *encoder) bool(v bool) {
	if v {
		e.uint32(1)
	} else {
		e.uint32(0)
	}
}

// opaque writes variable-length opaque data.
func (e *encoder) opaque(b []byte) {
	e.uint32(uint32(len(b)))
	e.buf = append(e.buf, b...)
	e.buf = append(e.buf, make([]byte, pad(len(b)))...)
}

func (e *encoder) string(s string) { e.opaque([]byte(s)) }
