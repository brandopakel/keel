package core

import (
	"errors"
	"strconv"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: lists (phase 4 of docs/embedding-plan.md, converted as
// docs/phase4-family-recipe.md converts a family).
//
// Each operation runs in a command's scope, after the type check of the key
// it names, and stages the record of every write it makes, so the list
// commands are never logged as they arrived (see strings.go). What the
// as-arrived log held is what they stage, byte for byte: a client's integers
// are canonical (canonicalInteger), so an index or a count written back in
// decimal is the argument the client sent.
//
// Where Redis answers a missing key before it reads the arguments after the
// key, or must build its reply before it writes, the lookup (listFor) is the
// command language's, split from the operation as SET's lookup is split from
// its write: LPOP and RPOP admit the elements' reply before they remove them,
// so a reply that cannot be sent leaves the list as it was.
//
// A list has no empty form: whatever empties one deletes the key
// (dropListIfEmpty).

var (
	// errNoSuchKey is LSET on a key that holds nothing, in Redis's words.
	errNoSuchKey = errors.New("ERR no such key")
	// errIndexOutOfRange is LSET past either end of the list.
	errIndexOutOfRange = errors.New("ERR index out of range")
)

// listFor is the list key holds, and false when it holds none. A key whose
// expiry has passed is reaped, and holds none.
func (e *Engine) listFor(key string) (*data_structure.List, bool) {
	return e.listStore.Get(key)
}

// dropListIfEmpty deletes the key of a list a write emptied, and otherwise
// resizes the store's account of it. A key with no elements would answer
// EXISTS 1 and LRANGE nothing, and the rewrite would write it as an RPUSH with
// no values, which is a syntax error on replay.
func (e *Engine) dropListIfEmpty(key string, l *data_structure.List) {
	if l.Len() == 0 {
		e.listStore.Delete(key)
		return
	}
	e.listStore.Resize(key)
}

// opPush adds values at the head of the list key holds (front), each in turn,
// so that LPUSH k a b leaves b first, or at its tail, creating the list if
// key holds none, and answers the list's length after it: LPUSH and RPUSH.
func (e *Engine) opPush(key string, values []string, front bool) int64 {
	l, ok := e.listFor(key)
	if !ok {
		l = data_structure.NewList()
		e.listStore.Put(key, l)
	}
	name := "RPUSH"
	if front {
		name = "LPUSH"
		l.PushFront(values...)
	} else {
		l.PushBack(values...)
	}
	e.listStore.Resize(key)
	e.aofRecordKeyArgs(name, key, values)
	return int64(l.Len())
}

// listPop is what an LPOP or RPOP asks for: the end it pops from, and with
// a count, how many elements.
type listPop struct {
	front   bool
	counted bool
	count   int64
}

// name is the command the pop is recorded as.
func (p listPop) name() string {
	if p.front {
		return "LPOP"
	}
	return "RPOP"
}

// popped is how many elements the pop takes from a list of n: one, or its
// count, at most the whole list.
func (p listPop) popped(n int) int {
	if !p.counted {
		return min(1, n)
	}
	return int(min(p.count, int64(n)))
}

// opPop removes what req asks for from l, the list key holds, once the
// command language has answered with those elements, and deletes the key if
// that empties it.
//
// A pop is recorded as it was asked for, its count included, as the log has
// always held it - also when l is nil because key holds no list, and when
// the count is zero, which change nothing. Redis propagates neither
// (t_list.c, popGenericCommand: no server.dirty for a missing key or a zero
// count); the records replay as the no-ops they are.
func (e *Engine) opPop(key string, l *data_structure.List, req listPop) {
	if l != nil {
		for i := req.popped(l.Len()); i > 0; i-- {
			if req.front {
				l.PopFront()
			} else {
				l.PopBack()
			}
		}
		e.dropListIfEmpty(key, l)
	}
	if req.counted {
		e.aofRecordInt(req.name(), key, req.count)
	} else {
		e.aofRecord(req.name(), key)
	}
}

// opLLen is the length of the list key holds, zero when it holds none: LLEN.
func (e *Engine) opLLen(key string) int64 {
	if l, ok := e.listFor(key); ok {
		return int64(l.Len())
	}
	return 0
}

// opLIndex is the element at index of the list key holds, counting from the
// tail when index is negative, and false when key holds no list or the index
// is past either end: LINDEX.
func (e *Engine) opLIndex(key string, index int64) (string, bool) {
	l, ok := e.listFor(key)
	if !ok {
		return "", false
	}
	return l.Index(int(index))
}

// listRange is the elements of a list from start to stop, resolved as LRANGE
// resolves them, and read where they are rather than copied.
type listRange struct {
	l           *data_structure.List
	start, stop int
}

// visit yields the range's elements in order, until yield returns false.
func (r listRange) visit(yield func(string) bool) { r.l.VisitRange(r.start, r.stop, yield) }

// opLRange is the elements of the list key holds from start to stop, both
// inclusive, counting from the tail when negative, and empty when the range
// falls outside the list; false when key holds no list: LRANGE. Its lookup
// is a keyspace hit or a miss, counted here, after the command language has
// read start and stop, as Redis's lrangeCommand reads them before it looks
// (readsAfterArguments).
func (e *Engine) opLRange(key string, start, stop int) (listRange, bool) {
	l, ok := e.listFor(key)
	e.noteLookup(ok)
	return listRange{l: l, start: start, stop: stop}, ok
}

// opLSet replaces the element at index of the list key holds, counting from
// the tail when index is negative: LSET. A key that holds no list is
// errNoSuchKey, and an index past either end errIndexOutOfRange; neither
// changes anything.
func (e *Engine) opLSet(key string, index int64, value string) error {
	l, ok := e.listFor(key)
	if !ok {
		return errNoSuchKey
	}
	if !l.Set(int(index), value) {
		return errIndexOutOfRange
	}
	e.listStore.Resize(key)
	e.aofRecord("LSET", key, strconv.FormatInt(index, 10), value)
	return nil
}

// opLTrim keeps the elements of the list key holds from start to stop, both
// inclusive and resolved as LRANGE resolves them, deleting the key when
// nothing is left: LTRIM. The list keeps its expiry.
//
// A trim is recorded as it was asked for, also when key holds no list, as
// the log has always held it. Redis propagates nothing then (t_list.c,
// ltrimCommand: a missing key returns before server.dirty); the record
// replays as the no-op it is.
func (e *Engine) opLTrim(key string, start, stop int) {
	if l, ok := e.listFor(key); ok {
		values := l.Range(start, stop)
		if len(values) == 0 {
			e.listStore.Delete(key)
		} else {
			ttl, expires := e.listStore.GetExpiry(key)
			replacement := data_structure.NewList()
			replacement.PushBack(values...)
			e.listStore.Put(key, replacement)
			if expires {
				e.listStore.SetExpiryAt(key, ttl)
			}
		}
	}
	e.aofRecord("LTRIM", key, strconv.Itoa(start), strconv.Itoa(stop))
}
