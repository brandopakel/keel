package data_structure

import (
	"time"
)

type Obj struct {
	Value        interface{}
	TypeEncoding uint8
	// type    | encoding
	// [][][][]|[][][][]

	// Access carries whatever bookkeeping the eviction policy needs, and what
	// it means depends on the policy in force:
	//
	//	LRU  the logical clock value at the last access
	//	LFU  a decay timestamp in the high 56 bits, and a logarithmic
	//	     frequency counter in the low 8
	//
	// One overloaded field rather than three honest ones because this is per
	// key, so its width is multiplied by the size of the keyspace: separate
	// fields measured 48 bytes per object against 32, which is 76MB at the
	// default five million key limit. Redis overloads its own lru field the
	// same way and for the same reason.
	Access uint64
}

type Dict struct {
	dictStore map[string]*Obj

	// expiredDictStore holds the instant each key with a TTL falls due, keyed
	// by the key's own name.
	//
	// It used to be keyed by the object pointer, which had two costs. Put
	// replaced an object without removing the old one's entry, so every
	// overwrite of a key with a TTL leaked an entry and kept the dead object
	// alive with it. And nothing could get from an expiry back to the name it
	// belonged to, which is exactly what a cycle sampling for expired keys
	// needs in order to delete one.
	expiredDictStore map[string]uint64

	// memUsed is the estimated bytes held, maintained incrementally: totalling
	// it on demand would be O(n) and a budget check runs on every write.
	memUsed uint64
}

func CreateDict() *Dict {
	return &Dict{
		dictStore:        make(map[string]*Obj),
		expiredDictStore: make(map[string]uint64),
	}
}

// NewObj builds a value for the dictionary.
//
// It no longer takes a TTL. Expiry is recorded against the key, and an object
// does not know its own name until it is put somewhere - so a caller that wants
// one sets it after the Put, which is also the order that makes SET clear a
// previous expiry and then apply the new one.
func (d *Dict) NewObj(value interface{}, oType uint8, oEnc uint8) *Obj {
	obj := &Obj{
		Value:        value,
		TypeEncoding: oType | oEnc,
	}
	obj.Access = NewAccess()
	return obj
}

func (d *Dict) HasExpired(k string) bool {
	exp, exist := d.expiredDictStore[k]
	if !exist {
		return false
	}
	return exp <= uint64(time.Now().UnixMilli())
}

func (d *Dict) GetExpiry(k string) (uint64, bool) {
	exp, exist := d.expiredDictStore[k]
	return exp, exist
}

func (d *Dict) SetExpiry(k string, ttlMs int64) {
	d.SetExpiryAt(k, uint64(time.Now().UnixMilli())+uint64(ttlMs))
}

// SetExpiryAt sets the expiry to an absolute time in milliseconds since the
// epoch. Persistence needs this: a relative TTL written to a log becomes a new
// TTL every time the log is replayed.
func (d *Dict) SetExpiryAt(k string, atMs uint64) {
	// Giving a key an expiry costs a second map entry, and entryBytes charges
	// for it - so the charge has to be made here, when the entry appears.
	//
	// It used to arrive with the object, before the Put that accounted for it,
	// and moving expiry onto the key moved it after. The delete side still
	// subtracted the overhead, so memUsed came out lower than it went in, and
	// on an unsigned counter that is not a small error: used_memory read 18
	// exabytes, and a maxmemory bound compared against it would have evicted
	// the entire keyspace on the next write.
	if _, existed := d.expiredDictStore[k]; !existed {
		d.memUsed += expiryOverhead
	}
	d.expiredDictStore[k] = atMs
}

// ExpiryOf reports a key's absolute expiry, for a log that has to record when
// rather than how much longer.
func (d *Dict) ExpiryOf(k string) (uint64, bool) {
	if _, ok := d.dictStore[k]; !ok {
		return 0, false
	}
	at, has := d.expiredDictStore[k]
	return at, has
}

// ExpiryCount is how many keys carry a TTL, so a test can check the table does
// not accumulate entries for keys that have gone.
func (d *Dict) ExpiryCount() int { return len(d.expiredDictStore) }

func (d *Dict) Get(k string) *Obj {
	v := d.dictStore[k]
	if v != nil {
		if d.HasExpired(k) {
			d.Del(k)
			// Reaping a key whose TTL has passed is a decision this server
			// made at a moment a log has to be able to reproduce. Without it
			// the key comes back on replay carrying an expiry that has already
			// gone by, and lives until something next reads it.
			noteRemoval(d, k)
			return nil
		}
		Touch(&v.Access)
	}
	return v
}

func (d *Dict) Put(k string, obj *Obj) {
	// An overwrite replaces the old value's cost rather than adding to it, so
	// its bytes are returned first. Under a key-count bound this is also why
	// overwriting must not evict: the dictionary does not grow.
	if old, exists := d.dictStore[k]; exists {
		d.memUsed -= d.entryBytes(k, old)
	}
	// A write replaces the value and, with it, any expiry the key had. That is
	// Redis's rule for SET without KEEPTTL, and it is also what stops the
	// expiry table growing an entry per overwrite.
	delete(d.expiredDictStore, k)

	Touch(&obj.Access)
	d.dictStore[k] = obj
	d.memUsed += d.entryBytes(k, obj)

	// Enforced after the insert rather than before, because what has to fit is
	// known exactly only once it is in. The key just written is the most
	// recently used and the most frequently accessed, so no policy will choose
	// it while anything else remains.
	EnforceLimits()
}

// Has reports whether a live key is present.
//
// A key whose expiry has passed is reaped here rather than reported, because
// the caller is asking who owns the name and a dead key owns nothing - leaving
// it would mean refusing to let another type take a name that is in truth free.
func (d *Dict) Has(k string) bool {
	if _, ok := d.dictStore[k]; !ok {
		return false
	}
	if d.HasExpired(k) {
		d.Del(k)
		noteRemoval(d, k)
		return false
	}
	return true
}

// Keys lists every key held, expired ones included: a rewrite reads each one
// through Peek, which is where the expiry is noticed.
func (d *Dict) Keys() []string {
	keys := make([]string, 0, len(d.dictStore))
	for k := range d.dictStore {
		keys = append(keys, k)
	}
	return keys
}

// Peek returns the object at a key without recording an access and without
// reaping it, for a caller that is reading the keyspace rather than using it.
// An expired key reads as absent, so a rewrite does not carry it forward.
func (d *Dict) Peek(k string) *Obj {
	obj, ok := d.dictStore[k]
	if !ok || d.HasExpired(k) {
		return nil
	}
	return obj
}

// Len reports how many keys are stored.
func (d *Dict) Len() int { return len(d.dictStore) }

func (d *Dict) Del(k string) bool {
	if obj, exist := d.dictStore[k]; exist {
		d.memUsed -= d.entryBytes(k, obj)
		delete(d.dictStore, k)
		delete(d.expiredDictStore, k)
		return true
	}
	return false
}

// Dict is a Keyspace, so eviction can weigh a string key against a set or a
// sketch on the same scale.

func (d *Dict) KeyspaceName() string { return "string" }

func (d *Dict) MemUsed() uint64 { return d.memUsed }

func (d *Dict) ScoreOf(key string) (uint64, bool) {
	obj, exists := d.dictStore[key]
	if !exists {
		return 0, false
	}
	return Score(obj.Access), true
}

// SampleKeys draws up to n keys at random.
//
// The randomness comes from ranging over the map, which Go deliberately starts
// at a random bucket. Consecutive keys are neighbours rather than independent
// draws - Redis's own dictGetSomeKeys has the same property - but the starting
// point moves every time, which is what the sampling needs.
func (d *Dict) SampleKeys(dst []Candidate, n int) []Candidate {
	taken := 0
	for key, obj := range d.dictStore {
		dst = append(dst, Candidate{Space: d, Key: key, Score: Score(obj.Access)})
		if taken++; taken >= n {
			break
		}
	}
	return dst
}

func (d *Dict) Delete(key string) bool { return d.Del(key) }
