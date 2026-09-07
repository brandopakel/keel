package core

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/brandopakel/keel/internal/data_structure"
)

// The table's shape and header are captured once; cells are read only by the
// event-loop owner in later slices. An intervening write dirties the key and
// requires a complete replacement before the rewrite can be published. Unlike
// dynamic filters, these tables cannot resize in place. Every intermediate
// image therefore remains structurally valid, with a checksum of emitted bytes.
type sketchDumpStream struct {
	image    *data_structure.SketchImage
	tag      byte
	checksum uint32
}

func (s *sketchDumpStream) size() int { return s.image.Size() + 9 }

func (s *sketchDumpStream) appendSlice(dst []byte, offset, count int) []byte {
	start := len(dst)
	if offset < 5 {
		header := [5]byte{'K', 'E', 'L', '1', s.tag}
		n := min(count, 5-offset)
		dst = append(dst, header[offset:offset+n]...)
		offset, count = offset+n, count-n
	}
	checksumAt := 5 + s.image.Size()
	if count > 0 && offset < checksumAt {
		n := min(count, checksumAt-offset)
		dst = s.image.AppendRange(dst, offset-5, n)
		offset, count = offset+n, count-n
	}
	s.checksum = crc32.Update(s.checksum, crc32.IEEETable, dst[start:])
	if count > 0 {
		var checksum [4]byte
		binary.LittleEndian.PutUint32(checksum[:], s.checksum)
		at := offset - checksumAt
		dst = append(dst, checksum[at:at+count]...)
	}
	return dst
}

func newSketchDumpStream(key string) *sketchDumpStream {
	if cms, ok := cmsStore.Peek(key); ok {
		return &sketchDumpStream{image: cms.RewriteImage(), tag: dumpTagCMS}
	}
	if morris, ok := morrisStore.Peek(key); ok {
		return &sketchDumpStream{image: morris.RewriteImage(), tag: dumpTagMorris}
	}
	return nil
}
