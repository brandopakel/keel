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
	dictStore        map[string]*Obj
	expiredDictStore map[*Obj]uint64

	// memUsed is the estimated bytes held, maintained incrementally: totalling
	// it on demand would be O(n) and a budget check runs on every write.
	memUsed uint64
}

func CreateDict() *Dict {
	return &Dict{
		dictStore:        make(map[string]*Obj),
		expiredDictStore: make(map[*Obj]uint64),
	}
}

func (d *Dict) NewObj(value interface{}, ttlMs int64, oType uint8, oEnc uint8) *Obj {
	obj := &Obj{
		Value:        value,
		TypeEncoding: oType | oEnc,
	}
	obj.Access = NewAccess()
	if ttlMs > 0 {
		d.SetExpiry(obj, ttlMs)
	}
	return obj
}

func (d *Dict) HasExpired(obj *Obj) bool {
	exp, exist := d.expiredDictStore[obj]
	if !exist {
		return false
	}
	return exp <= uint64(time.Now().UnixMilli())
}

func (d *Dict) GetExpiry(obj *Obj) (uint64, bool) {
	exp, exist := d.expiredDictStore[obj]
	return exp, exist
}

func (d *Dict) SetExpiry(obj *Obj, ttlMs int64) {
	d.SetExpiryAt(obj, uint64(time.Now().UnixMilli())+uint64(ttlMs))
}

// SetExpiryAt sets the expiry to an absolute time in milliseconds since the
// epoch. Persistence needs this: a relative TTL written to a log becomes a new
// TTL every time the log is replayed.
func (d *Dict) SetExpiryAt(obj *Obj, atMs uint64) {
	d.expiredDictStore[obj] = atMs
}

// ExpiryOf reports a key's absolute expiry, for a log that has to record when
// rather than how much longer.
func (d *Dict) ExpiryOf(k string) (uint64, bool) {
	obj, ok := d.dictStore[k]
	if !ok {
		return 0, false
	}
	at, has := d.expiredDictStore[obj]
	return at, has
}

func (d *Dict) Get(k string) *Obj {
	v := d.dictStore[k]
	if v != nil {
		if d.HasExpired(v) {
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
	obj, ok := d.dictStore[k]
	if !ok {
		return false
	}
	if d.HasExpired(obj) {
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
	if !ok || d.HasExpired(obj) {
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
		delete(d.expiredDictStore, obj)
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
