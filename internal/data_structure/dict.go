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
	d.expiredDictStore[obj] = uint64(time.Now().UnixMilli()) + uint64(ttlMs)
}

func (d *Dict) Get(k string) *Obj {
	v := d.dictStore[k]
	if v != nil {
		if d.HasExpired(v) {
			d.Del(k)
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
