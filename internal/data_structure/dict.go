package data_structure

import "time"

// Obj holds a string and its eviction metadata. Collection values live in
// separate typed keyspaces. Keeping the string header here avoids an interface
// box per value; there is no redundant type/encoding byte or its padding.
type Obj struct {
	Value string
	// Access is the LRU clock or the packed LFU decay/frequency word.
	Access uint64
}

// Dict is the string keyspace: the values, and the expiry of each key that has
// one.
type Dict struct {
	dictStore keyMap[Obj]

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
	return &Dict{expiredDictStore: map[string]uint64{}}
}

// NewObj builds a value for the dictionary.
//
// It no longer takes a TTL. Expiry is recorded against the key, and an object
// does not know its own name until it is put somewhere - so a caller that wants
// one sets it after the Put, which is also the order that makes SET clear a
// previous expiry and then apply the new one.
func (d *Dict) NewObj(value string) *Obj {
	return &Obj{
		Value:  value,
		Access: NewAccess(),
	}
}

// nowMs is the clock every expiry is compared against.
// SuspendExpiry keeps historical AOF mutations independent of the replay wall clock.
var SuspendExpiry bool

func nowMs() uint64 {
	if SuspendExpiry {
		return 0
	}
	return uint64(time.Now().UnixMilli())
}

// HasExpired reports whether a key has a TTL that has already passed.
func (d *Dict) HasExpired(k string) bool {
	at, has := d.expiredDictStore[k]
	return has && at <= nowMs()
}

// GetExpiry returns when a key falls due, and whether it has a TTL at all.
func (d *Dict) GetExpiry(k string) (uint64, bool) {
	at, has := d.expiredDictStore[k]
	return at, has
}

// SetExpiry gives a key ttlMs more milliseconds to live.
func (d *Dict) SetExpiry(k string, ttlMs int64) {
	d.SetExpiryAt(k, nowMs()+uint64(ttlMs))
}

// SetExpiryAt sets the expiry to an absolute time in milliseconds since the
// epoch. Persistence needs this: a relative TTL written to a log becomes a new
// TTL every time the log is replayed.
func (d *Dict) SetExpiryAt(k string, atMs uint64) {
	if _, ok := d.dictStore.getPtr(k); !ok {
		return
	}
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
	EnforceLimits()
}

// ExpiryOf reports a key's absolute expiry, for a log that has to record when
// rather than how much longer.
func (d *Dict) ExpiryOf(k string) (uint64, bool) {
	if _, ok := d.dictStore.getPtr(k); !ok {
		return 0, false
	}
	at, has := d.expiredDictStore[k]
	return at, has
}

// ExpiryCount is how many keys carry a TTL, so a test can check the table does
// not accumulate entries for keys that have gone.
func (d *Dict) ExpiryCount() int { return len(d.expiredDictStore) }

// Get returns the live object at a key and records the access. A key whose
// TTL has passed is reaped on the way and reads as absent.
func (d *Dict) Get(k string) *Obj {
	obj, ok := d.dictStore.getPtr(k)
	if !ok {
		return nil
	}
	if d.HasExpired(k) {
		d.Del(k)
		// Reaping a key whose TTL has passed is a decision this server
		// made at a moment a log has to be able to reproduce. Without it
		// the key comes back on replay carrying an expiry that has already
		// gone by, and lives until something next reads it.
		noteRemoval(d, k)
		return nil
	}
	Touch(&obj.Access)
	return obj
}

// Put stores obj at k, replacing whatever was there along with its expiry.
func (d *Dict) Put(k string, obj *Obj) {
	// An overwrite replaces the old value's cost rather than adding to it, so
	// its bytes are returned first. Under a key-count bound this is also why
	// overwriting must not evict: the dictionary does not grow.
	if old, exists := d.dictStore.getPtr(k); exists {
		d.memUsed -= d.entryBytes(k, old)
	}
	// A write replaces the value and, with it, any expiry the key had. That is
	// Redis's rule for SET without KEEPTTL, and it is also what stops the
	// expiry table growing an entry per overwrite.
	delete(d.expiredDictStore, k)

	Touch(&obj.Access)
	d.dictStore.set(k, *obj)
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
	if _, ok := d.dictStore.getPtr(k); !ok {
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
	return d.dictStore.keys()
}

// Peek returns the object at a key without recording an access and without
// reaping it, for a caller that is reading the keyspace rather than using it.
// An expired key reads as absent, so a rewrite does not carry it forward.
func (d *Dict) Peek(k string) *Obj {
	obj, ok := d.dictStore.getPtr(k)
	if !ok || d.HasExpired(k) {
		return nil
	}
	return obj
}

// Len reports how many keys are stored.
func (d *Dict) Len() int { return d.dictStore.len() }

// Scan hands the walk to the shards. See sharded.go for why the cursor is a
// shard index and what that does and does not promise.
func (d *Dict) Scan(cursor uint64, budget int, keep func(string) bool, dst []string) ([]string, int, uint64) {
	return d.dictStore.scan(cursor, budget, keep, dst)
}

func (d *Dict) ScanEnd() uint64 { return d.dictStore.scanEnd() }
func (d *Dict) ScanUntil(cursor, end uint64, budget int, keep func(string) bool, dst []string) ([]string, int, uint64) {
	return d.dictStore.scanUntil(cursor, end, budget, keep, dst)
}

// Del removes a key, its expiry and its charge against the budget, and reports
// whether there was anything to remove.
func (d *Dict) Del(k string) bool {
	obj, ok := d.dictStore.getPtr(k)
	if !ok {
		return false
	}
	d.memUsed -= d.entryBytes(k, obj)
	d.dictStore.del(k)
	delete(d.expiredDictStore, k)
	return true
}

// Dict is a Keyspace, so eviction can weigh a string key against a set or a
// sketch on the same scale.

func (d *Dict) KeyspaceName() string { return "string" }

func (d *Dict) MemUsed() uint64 { return d.memUsed }

func (d *Dict) ScoreOf(key string) (uint64, bool) {
	obj, exists := d.dictStore.getPtr(key)
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
	d.dictStore.sample(n, func(key string, obj Obj) {
		dst = append(dst, Candidate{Space: d, Key: key, Score: Score(obj.Access)})
	})
	return dst
}

func (d *Dict) Delete(key string) bool { return d.Del(key) }

// UpdateValue accounts an in-place value change while preserving its expiry.
func (d *Dict) UpdateValue(key string, value string) {
	obj, ok := d.dictStore.getPtr(key)
	if !ok || obj == nil {
		return
	}
	d.memUsed -= d.entryBytes(key, obj)
	obj.Value = value
	d.memUsed += d.entryBytes(key, obj)
	EnforceLimits()
}

func (d *Dict) ClearExpiry(key string) bool {
	if _, ok := d.expiredDictStore[key]; !ok {
		return false
	}
	delete(d.expiredDictStore, key)
	d.memUsed -= expiryOverhead
	return true
}
