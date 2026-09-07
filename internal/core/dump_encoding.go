package core

import (
	"encoding/binary"
	"hash/crc32"
	"strconv"
)

type dumpPlan struct {
	tag        byte
	size       int // body only; the version, type and checksum add nine bytes
	appendBody func([]byte) []byte
}

func appendDump(dst []byte, plan dumpPlan) []byte {
	start := len(dst)
	dst = append(dst, 'K', 'E', 'L', '1', plan.tag)
	dst = plan.appendBody(dst)
	return binary.LittleEndian.AppendUint32(dst, crc32.ChecksumIEEE(dst[start:]))
}

// Commands execute atomically, so values cannot grow between sizing and encoding.
// Collection walkers retain no member arrays. An oversized body stops sizing
// early and is represented by limit+1; callers reject it before construction.
func partsDumpPlan(tag byte, walk replyWalk, limit int) dumpPlan {
	size := 0
	walk(func(value string) bool {
		if len(value) > limit-size-4 {
			size = limit + 1
			return false
		}
		size += 4 + len(value)
		return true
	})
	return dumpPlan{tag, size, func(dst []byte) []byte {
		w := respParts{b: dst}
		walk(func(value string) bool { w.add(value); return true })
		return w.b
	}}
}

type dumpMarshaler interface {
	MarshalSize() int
	AppendMarshal([]byte) []byte
}

func opaqueDumpPlan(tag byte, value dumpMarshaler) dumpPlan {
	return dumpPlan{tag, value.MarshalSize(), value.AppendMarshal}
}

func planDump(key string, limit int) (dumpPlan, bool) {
	if h, ok := hashStore.Peek(key); ok {
		return partsDumpPlan(dumpTagHash, func(yield func(string) bool) {
			h.Visit(func(field, value string) bool { return yield(field) && yield(value) })
		}, limit), true
	}
	if l, ok := listStore.Peek(key); ok {
		return partsDumpPlan(dumpTagList, func(yield func(string) bool) {
			l.VisitRange(0, l.Len()-1, yield)
		}, limit), true
	}
	if obj := dictStore.Peek(key); obj != nil {
		return dumpPlan{dumpTagString, len(obj.Value), func(dst []byte) []byte {
			return append(dst, obj.Value...)
		}}, true
	}
	if set, ok := setStore.Peek(key); ok {
		return partsDumpPlan(dumpTagSet, func(yield func(string) bool) {
			for i := 0; i < set.Len(); i++ {
				value, _ := set.MemberAt(i)
				if !yield(value) {
					return
				}
			}
		}, limit), true
	}
	if z, ok := zsetStore.Peek(key); ok {
		size := 0
		var scoreBuffer [32]byte
		z.VisitRangeByRank(0, z.Len()-1, false, func(member string, score float64) bool {
			width := len(strconv.AppendFloat(scoreBuffer[:0], score, 'g', -1, 64))
			if len(member) > limit-size-8-width {
				size = limit + 1
				return false
			}
			size += 8 + len(member) + width
			return true
		})
		return dumpPlan{dumpTagZSet, size, func(dst []byte) []byte {
			z.VisitRangeByRank(0, z.Len()-1, false, func(member string, score float64) bool {
				start := len(dst)
				dst = binary.LittleEndian.AppendUint32(dst, 0)
				dst = strconv.AppendFloat(dst, score, 'g', -1, 64)
				binary.LittleEndian.PutUint32(dst[start:start+4], uint32(len(dst)-start-4))
				dst = binary.LittleEndian.AppendUint32(dst, uint32(len(member)))
				dst = append(dst, member...)
				return true
			})
			return dst
		}}, true
	}
	return planOpaqueDump(key)
}

func planOpaqueDump(key string) (dumpPlan, bool) {
	if v, ok := sbStore.Peek(key); ok {
		return opaqueDumpPlan(dumpTagBloom, v), true
	}
	if v, ok := cmsStore.Peek(key); ok {
		return opaqueDumpPlan(dumpTagCMS, v), true
	}
	if v, ok := morrisStore.Peek(key); ok {
		return opaqueDumpPlan(dumpTagMorris, v), true
	}
	if v, ok := hllStore.Peek(key); ok {
		return opaqueDumpPlan(dumpTagHLL, v), true
	}
	if v, ok := cfStore.Peek(key); ok {
		return opaqueDumpPlan(dumpTagCuckoo, v), true
	}
	return dumpPlan{}, false
}
