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

	// LastAccess is the value of the dictionary's logical clock when this
	// object was last read or written. Approximate LRU compares these.
	LastAccess uint64
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
}

func CreateDict() *Dict {
	res := Dict{
		dictStore:        make(map[string]*Obj),
		expiredDictStore: make(map[*Obj]uint64),
		pool:             make([]poolEntry, 0, evictionPoolSize),
	}
	return &res
}

// touch records an access, which is what makes a key recently used.
func (d *Dict) touch(obj *Obj) {
	d.clock++
	obj.LastAccess = d.clock
}

func (d *Dict) NewObj(value interface{}, ttlMs int64, oType uint8, oEnc uint8) *Obj {
	obj := &Obj{
		Value:        value,
		TypeEncoding: oType | oEnc,
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
	case config.EvictFirst:
		d.evictFirst()
	default:
		d.evictFirst()
	}
}
