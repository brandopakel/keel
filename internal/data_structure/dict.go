package data_structure

import (
	"memkv/internal/config"
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

// The LRU reading of Access.
func (o *Obj) lruClock() uint64     { return o.Access }
func (o *Obj) setLRUClock(c uint64) { o.Access = c }

// The LFU reading of Access.
func (o *Obj) lfuFreq() uint8     { return uint8(o.Access) }
func (o *Obj) lfuDecayAt() uint64 { return o.Access >> 8 }
func (o *Obj) setLFU(decayAt uint64, freq uint8) {
	o.Access = decayAt<<8 | uint64(freq)
}

type Dict struct {
	dictStore        map[string]*Obj
	expiredDictStore map[*Obj]uint64

	// clock is a logical counter, incremented on every access, rather than a
	// wall clock. Redis stores a coarse seconds-resolution timestamp because it
	// has 24 bits to spend on it; a counter costs more per object but gives an
	// exact recency ordering, so any inaccuracy in eviction comes from the
	// sampling rather than from two keys sharing a timestamp.
	clock uint64

	// pool carries the best eviction candidates between evictions. See lru.go.
	pool []poolEntry

	// rngState drives the probabilistic counter increment under LFU. A
	// per-dictionary xorshift rather than math/rand, so behaviour is
	// reproducible and nothing touches a shared source.
	rngState uint64
}

func CreateDict() *Dict {
	res := Dict{
		dictStore:        make(map[string]*Obj),
		expiredDictStore: make(map[*Obj]uint64),
		pool:             make([]poolEntry, 0, evictionPoolSize),
		rngState:         0x2545F4914F6CDD1D,
	}
	return &res
}

func (d *Dict) nextRand() uint64 {
	d.rngState ^= d.rngState << 13
	d.rngState ^= d.rngState >> 7
	d.rngState ^= d.rngState << 17
	return d.rngState
}

// touch records an access. What that means depends on the policy: LRU wants to
// know when, LFU wants to know how often.
func (d *Dict) touch(obj *Obj) {
	d.clock++
	if config.EvictStrategy == config.LFU {
		d.touchLFU(obj)
		return
	}
	obj.setLRUClock(d.clock)
}

func (d *Dict) NewObj(value interface{}, ttlMs int64, oType uint8, oEnc uint8) *Obj {
	obj := &Obj{
		Value:        value,
		TypeEncoding: oType | oEnc,
	}
	if config.EvictStrategy == config.LFU {
		// A new key starts with credit rather than at zero. Without it, every
		// new key is the least frequently used thing in the dictionary and is
		// evicted before it can demonstrate otherwise - so nothing new ever
		// survives, however popular it is about to become.
		obj.setLFU(d.clock, lfuInitVal)
	}
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
		d.touch(v)
	}
	return v
}

func (d *Dict) Put(k string, obj *Obj) {
	// Evict before inserting, and only when the key is genuinely new:
	// overwriting an existing key does not grow the dictionary, so evicting for
	// it would throw away a key for nothing.
	if _, exists := d.dictStore[k]; !exists && len(d.dictStore) >= config.KeyNumberLimit {
		d.evict()
	}
	d.touch(obj)
	d.dictStore[k] = obj
}

// Len reports how many keys are stored.
func (d *Dict) Len() int { return len(d.dictStore) }

func (d *Dict) Del(k string) bool {
	if obj, exist := d.dictStore[k]; exist {
		delete(d.dictStore, k)
		delete(d.expiredDictStore, obj)
		return true
	}
	return false
}

func (d *Dict) evictFirst() {
	for k := range d.dictStore {
		d.Del(k)
		return
	}
}

func (d *Dict) evict() {
	switch config.EvictStrategy {
	case config.LRU:
		d.evictApproxLRU()
	case config.LFU:
		d.evictApproxLFU()
	case config.EvictFirst:
		d.evictFirst()
	default:
		d.evictFirst()
	}
}
