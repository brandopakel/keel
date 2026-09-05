package core

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
	"github.com/brandopakel/keel/internal/data_structure"
)

// KEEL.DUMP and KEEL.RESTORE move a key's whole state as bytes.
//
// They exist because rewriting the append-only file needs a way to write a
// HyperLogLog, a filter or a sketch, and there is no command that rebuilds one:
// its state came from hashing items that were deliberately never stored. A set
// can be written back as SADD of its members and a string as SET; these five
// have to be written as themselves.
//
// Named KEEL.* rather than DUMP and RESTORE because the payload is not Redis's
// format - no version footer, no CRC64 - and taking those names would promise a
// compatibility that is not there. A Redis payload will not load here and a
// payload from here will not load there.
//
// MEMKV.DUMP and MEMKV.RESTORE are still accepted, because the server was
// called memkv until it was called keel and every append-only file written
// before the rename records MEMKV.RESTORE. Dropping the old name would turn
// those logs into a startup error, or worse into a silently shorter keyspace.
// Only the new name is ever written.
//
// New payloads have a KEL1 version prefix and CRC32 checksum. Legacy unversioned
// payloads remain readable. Neither form includes the key's expiry deadline.
const (
	dumpTagString = byte(1)
	dumpTagSet    = byte(2)
	dumpTagZSet   = byte(3)
	dumpTagBloom  = byte(4)
	dumpTagCMS    = byte(5)
	dumpTagMorris = byte(6)
	dumpTagHLL    = byte(7)
	dumpTagCuckoo = byte(8)
	dumpTagHash   = byte(9)
	dumpTagList   = byte(10)
)

// dumpKey serialises whatever holds the key.
func dumpKey(key string) ([]byte, bool) {
	payload, ok := dumpValue(key)
	if !ok {
		return nil, false
	}
	out := append([]byte("KEL1"), payload...)
	out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
	return out, true
}

func dumpValue(key string) ([]byte, bool) {
	if h, ok := hashStore.Peek(key); ok {
		w := &respParts{}
		fs, vs := h.Entries()
		for i, f := range fs {
			w.add(f)
			w.add(vs[i])
		}
		return append([]byte{dumpTagHash}, w.encode()...), true
	}
	if l, ok := listStore.Peek(key); ok {
		w := &respParts{}
		for _, v := range l.All() {
			w.add(v)
		}
		return append([]byte{dumpTagList}, w.encode()...), true
	}
	// Peek, not Get. Get records an access and reaps an expired key, and this
	// is called for every key of a rewrite: reading the keyspace would mark all
	// of it recently used and leave eviction with no idea which keys anyone
	// actually wanted. Dumping a key is not using it.
	if obj := dictStore.Peek(key); obj != nil {
		if s, ok := obj.Value.(string); ok {
			return append([]byte{dumpTagString}, s...), true
		}
	}
	if set, ok := setStore.Peek(key); ok {
		w := &respParts{}
		for _, m := range set.Members() {
			w.add(m)
		}
		return append([]byte{dumpTagSet}, w.encode()...), true
	}
	if zset, ok := zsetStore.Peek(key); ok {
		members, scores := zset.Entries()
		w := &respParts{}
		for i, m := range members {
			w.add(formatScore(scores[i]))
			w.add(m)
		}
		return append([]byte{dumpTagZSet}, w.encode()...), true
	}
	if sb, ok := sbStore.Peek(key); ok {
		return append([]byte{dumpTagBloom}, sb.Marshal()...), true
	}
	if cms, ok := cmsStore.Peek(key); ok {
		return append([]byte{dumpTagCMS}, cms.Marshal()...), true
	}
	if m, ok := morrisStore.Peek(key); ok {
		return append([]byte{dumpTagMorris}, m.Marshal()...), true
	}
	if h, ok := hllStore.Peek(key); ok {
		return append([]byte{dumpTagHLL}, h.Marshal()...), true
	}
	if cf, ok := cfStore.Peek(key); ok {
		return append([]byte{dumpTagCuckoo}, cf.Marshal()...), true
	}
	return nil, false
}

// restoreKey rebuilds a key from a payload, replacing whatever was there.
//
// The payload is decoded in full before anything is touched, so a payload that
// turns out to be malformed leaves the key exactly as it was. Deleting first
// and decoding second lost the old value on every bad payload.
func restoreKey(key string, payload []byte) error {
	if err := affordable(uint64(len(payload))); err != nil {
		return err
	}
	if bytes.HasPrefix(payload, []byte("KEL")) {
		if len(payload) < 9 || string(payload[:4]) != "KEL1" {
			return errors.New("ERR unsupported dump version")
		}
		end := len(payload) - 4
		if crc32.ChecksumIEEE(payload[:end]) != binary.LittleEndian.Uint32(payload[end:]) {
			return errors.New("ERR dump checksum mismatch")
		}
		payload = payload[4:end]
	}
	if len(payload) == 0 {
		return errors.New("MEMKV: empty payload")
	}
	store, err := decodeRestorePayload(key, payload[0], payload[1:])
	if err != nil {
		return err
	}
	// Whatever type used to hold this name gives it up, or restoring a set over
	// a string would leave both - the bug the keyspace check exists to prevent.
	data_structure.DeleteAnywhere(key)
	store()
	return nil
}

// decodeRestorePayload turns a payload into the value it describes and returns
// the step that puts that value in its keyspace, having changed nothing yet.
func decodeRestorePayload(key string, tag byte, body []byte) (store func(), err error) {
	switch tag {
	case dumpTagHash, dumpTagList:
		parts, err := decodeParts(body)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 || (tag == dumpTagHash && len(parts)%2 != 0) {
			return nil, errors.New("ERR invalid collection payload")
		}
		if tag == dumpTagHash {
			h := data_structure.NewHash()
			for i := 0; i < len(parts); i += 2 {
				h.Set(parts[i], parts[i+1])
			}
			return func() { hashStore.Put(key, h) }, nil
		}
		l := data_structure.NewList()
		l.PushBack(parts...)
		return func() { listStore.Put(key, l) }, nil
	case dumpTagString:
		value := string(body)
		oType, oEnc := deduceTypeString(value)
		return func() { dictStore.Put(key, dictStore.NewObj(value, oType, oEnc)) }, nil
	case dumpTagSet:
		members, err := decodeParts(body)
		if err != nil {
			return nil, err
		}
		if len(members) == 0 {
			return nil, errors.New("ERR empty set payload")
		}
		set := data_structure.NewSet()
		if len(members) > 0 {
			set.Add(members...)
		}
		return func() { setStore.Put(key, set) }, nil
	case dumpTagZSet:
		parts, err := decodeParts(body)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 || len(parts)%2 != 0 {
			return nil, errors.New("MEMKV: sorted set payload is not score/member pairs")
		}
		zset := data_structure.CreateZSet()
		for i := 0; i < len(parts); i += 2 {
			score, perr := parseScore(parts[i])
			if perr != nil {
				return nil, perr
			}
			zset.Add(score, parts[i+1], 0)
		}
		return func() { zsetStore.Put(key, zset) }, nil
	case dumpTagBloom:
		sb, err := data_structure.UnmarshalSBChain(body)
		if err != nil {
			return nil, err
		}
		return func() { sbStore.Put(key, sb) }, nil
	case dumpTagCMS:
		cms, err := data_structure.UnmarshalCMS(body)
		if err != nil {
			return nil, err
		}
		return func() { cmsStore.Put(key, cms) }, nil
	case dumpTagMorris:
		m, err := data_structure.UnmarshalMorris(body)
		if err != nil {
			return nil, err
		}
		return func() { morrisStore.Put(key, m) }, nil
	case dumpTagHLL:
		h, err := data_structure.UnmarshalHLL(body)
		if err != nil {
			return nil, err
		}
		return func() { hllStore.Put(key, h) }, nil
	case dumpTagCuckoo:
		cf, err := data_structure.UnmarshalCuckoo(body)
		if err != nil {
			return nil, err
		}
		return func() { cfStore.Put(key, cf) }, nil
	}
	return nil, fmt.Errorf("MEMKV: unknown payload type %d", tag)
}

func cmdDUMP(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'KEEL.DUMP' command"), false)
	}
	payload, ok := dumpKey(args[0])
	if !ok {
		return constant.RespNil
	}
	return Encode(string(payload), false)
}

func cmdRESTORE(args []string) []byte {
	if len(args) != 2 {
		return Encode(errors.New("(error) ERR wrong number of arguments for 'KEEL.RESTORE' command"), false)
	}
	if err := restoreKey(args[0], []byte(args[1])); err != nil {
		return Encode(err, false)
	}
	return constant.RespOk
}

// formatScore and parseScore round-trip a score exactly. 'g' with -1 precision
// prints the shortest text that parses back to the same float64, which is what
// a payload needs: a score written and read again must be bit-identical, or a
// sorted set would drift a little on every rewrite.
func formatScore(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func parseScore(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	// NaN parses, and is not a score: a member restored under it could never
	// be found in the skip list again.
	if err != nil || math.IsNaN(f) {
		return 0, fmt.Errorf("MEMKV: bad score %q", s)
	}
	return f, nil
}

// respParts packs a list of strings into one payload.
//
// Members and scores are variable length and may contain anything, so they are
// length-prefixed rather than delimited - a separator would need escaping, and
// a set member is allowed to contain any byte a separator could be.
type respParts struct{ b []byte }

func (w *respParts) add(s string) {
	var n [4]byte
	n[0], n[1], n[2], n[3] = byte(len(s)), byte(len(s)>>8), byte(len(s)>>16), byte(len(s)>>24)
	w.b = append(w.b, n[:]...)
	w.b = append(w.b, s...)
}

func (w *respParts) encode() []byte { return w.b }

func decodeParts(p []byte) ([]string, error) {
	var out []string
	for len(p) > 0 {
		if len(p) < 4 {
			return nil, errors.New("MEMKV: payload ends inside a length")
		}
		n := int(p[0]) | int(p[1])<<8 | int(p[2])<<16 | int(p[3])<<24
		p = p[4:]
		if n < 0 || n > len(p) {
			return nil, errors.New("MEMKV: payload ends inside a value")
		}
		out = append(out, string(p[:n]))
		p = p[n:]
	}
	return out, nil
}
