package data_structure

import "encoding/binary"

// SketchImage reads a structurally valid, fixed-size CMS/Morris representation
// in bounded pieces. It borrows the table, not a copy. The event-loop owner must
// perform every read and mutation serially and reconcile every intervening
// mutation before publishing a rewrite. This is not an immutable snapshot and
// must never be handed to a concurrent worker or returned directly to a client.
type SketchImage struct {
	header      [24]byte
	headerBytes int
	cells32     []uint32
	cells8      []uint8
}

func (c *CMS) RewriteImage() *SketchImage {
	r := &SketchImage{headerBytes: 16, cells32: c.counter}
	binary.LittleEndian.PutUint32(r.header[:4], c.width)
	binary.LittleEndian.PutUint32(r.header[4:8], c.depth)
	binary.LittleEndian.PutUint64(r.header[8:16], c.totalCount)
	return r
}

func (m *Morris) RewriteImage() *SketchImage {
	r := &SketchImage{headerBytes: 24, cells8: m.counters}
	binary.LittleEndian.PutUint32(r.header[:4], m.width)
	binary.LittleEndian.PutUint32(r.header[4:8], m.depth)
	binary.LittleEndian.PutUint64(r.header[8:16], m.totalCount)
	binary.LittleEndian.PutUint64(r.header[16:24], m.rngState)
	return r
}

func (r *SketchImage) Size() int { return r.headerBytes + 4*len(r.cells32) + len(r.cells8) }

// AppendRange appends up to count bytes at offset. CMS cells need not start or
// end on a four-byte boundary: RESP framing can leave any remaining slice size.
func (r *SketchImage) AppendRange(dst []byte, offset, count int) []byte {
	if offset < 0 || count <= 0 || offset >= r.Size() {
		return dst
	}
	count = min(count, r.Size()-offset)
	if offset < r.headerBytes {
		n := min(count, r.headerBytes-offset)
		dst = append(dst, r.header[offset:offset+n]...)
		offset, count = offset+n, count-n
	}
	if count == 0 {
		return dst
	}
	offset -= r.headerBytes
	if r.cells32 == nil {
		return append(dst, r.cells8[offset:offset+count]...)
	}
	for count > 0 && offset%4 != 0 {
		dst = append(dst, byte(r.cells32[offset/4]>>uint(8*(offset%4))))
		offset, count = offset+1, count-1
	}
	for count >= 4 {
		dst = binary.LittleEndian.AppendUint32(dst, r.cells32[offset/4])
		offset, count = offset+4, count-4
	}
	for count > 0 {
		dst = append(dst, byte(r.cells32[offset/4]>>uint(8*(offset%4))))
		offset, count = offset+1, count-1
	}
	return dst
}
