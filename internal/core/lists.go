package core

import (
	"errors"
	"slices"
	"strconv"
	"strings"

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
// command language has answered with those elements, deletes the key if that
// empties it, and records the pop as it was asked for, its count included.
//
// A pop that takes nothing changes nothing and records nothing, as Redis
// propagates nothing for it: a count of zero (t_list.c, popGenericCommand,
// lines 840-844), and a key that holds no list (line 836), which never
// reaches here.
func (e *Engine) opPop(key string, l *data_structure.List, req listPop) {
	n := req.popped(l.Len())
	if n == 0 {
		return
	}
	for ; n > 0; n-- {
		if req.front {
			l.PopFront()
		} else {
			l.PopBack()
		}
	}
	e.dropListIfEmpty(key, l)
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
// A trim that removes nothing changes nothing and records nothing, as Redis
// propagates nothing for it: a key that holds no list (t_list.c,
// ltrimCommand, line 946), and a range that covers the whole list, which adds
// nothing to server.dirty (line 994).
func (e *Engine) opLTrim(key string, start, stop int) {
	l, ok := e.listFor(key)
	if !ok {
		return
	}
	values := l.Range(start, stop)
	switch len(values) {
	case l.Len():
		return
	case 0:
		e.listStore.Delete(key)
	default:
		ttl, expires := e.listStore.GetExpiry(key)
		replacement := data_structure.NewList()
		replacement.PushBack(values...)
		e.listStore.Put(key, replacement)
		if expires {
			e.listStore.SetExpiryAt(key, ttl)
		}
	}
	e.aofRecord("LTRIM", key, strconv.Itoa(start), strconv.Itoa(stop))
}

// opPushX adds values to the list key holds as opPush does, and only if key
// holds one: LPUSHX and RPUSHX, which answer 0 and record nothing for a key
// that holds no list.
func (e *Engine) opPushX(key string, values []string, front bool) int64 {
	l, ok := e.listFor(key)
	if !ok {
		return 0
	}
	name := "RPUSHX"
	if front {
		name = "LPUSHX"
		l.PushFront(values...)
	} else {
		l.PushBack(values...)
	}
	e.listStore.Resize(key)
	e.aofRecordKeyArgs(name, key, values)
	return int64(l.Len())
}

// opLInsert puts element just before the first element from the head equal
// to pivot, or just after it, in the list key holds, and answers the list's
// length after it: LINSERT. It answers -1 when no element equals pivot, and
// 0 when key holds no list; neither changes anything or records anything.
func (e *Engine) opLInsert(key string, after bool, pivot, element string) int64 {
	l, ok := e.listFor(key)
	if !ok {
		return 0
	}
	at := -1
	for i := 0; i < l.Len(); i++ {
		if v, _ := l.Index(i); v == pivot {
			at = i
			break
		}
	}
	if at < 0 {
		return -1
	}
	where := "BEFORE"
	if after {
		at++
		where = "AFTER"
	}
	l.Insert(at, element)
	e.listStore.Resize(key)
	e.aofRecord("LINSERT", key, where, pivot, element)
	return int64(l.Len())
}

// opLRem removes the elements equal to element from the list key holds and
// answers how many it removed: LREM. A positive count removes at most that
// many from the head, a negative one from the tail, and zero every one. A
// list it empties is deleted. Removing nothing records nothing, as Redis
// counts each removal (t_list.c, lremCommand, line 1145).
func (e *Engine) opLRem(key string, count int64, element string) int64 {
	l, ok := e.listFor(key)
	if !ok {
		return 0
	}
	n, fromTail := count, false
	if count < 0 {
		n, fromTail = -count, true
	}
	removed := l.Remove(element, int(n), fromTail)
	if removed == 0 {
		return 0
	}
	e.dropListIfEmpty(key, l)
	e.aofRecord("LREM", key, strconv.FormatInt(count, 10), element)
	return int64(removed)
}

// lposRequest is what an LPOS asks for beyond the element.
type lposRequest struct {
	// rank is which match to start from, counting from the head, or from the
	// tail when negative; never zero.
	rank int64
	// count is how many matches to answer, every one when zero, and -1 when
	// COUNT was not given, which answers one match and not an array.
	count int64
	// maxLen is how many elements to compare at most, every one when zero.
	maxLen int64
}

// lposMatches is the positions of an element in a list, as an LPOS
// request selects them, found where they are rather than collected.
type lposMatches struct {
	l       *data_structure.List
	element string
	req     lposRequest
}

// visit yields the positions, counted from the head as LINDEX counts them,
// in the order the search finds them, until yield returns false.
func (m lposMatches) visit(yield func(int64) bool) {
	rank, fromTail := m.req.rank, false
	if rank < 0 {
		rank, fromTail = -rank, true
	}
	n := int64(m.l.Len())
	matches, found := int64(0), int64(0)
	for i := int64(0); i < n && (m.req.maxLen == 0 || i < m.req.maxLen); i++ {
		index := i
		if fromTail {
			index = n - 1 - i
		}
		if v, _ := m.l.Index(int(index)); v != m.element {
			continue
		}
		matches++
		if matches < rank {
			continue
		}
		found++
		if !yield(index) || m.req.count == -1 || (m.req.count > 0 && found >= m.req.count) {
			return
		}
	}
}

// opLPos is the positions of the elements equal to element in the list key
// holds, as req selects them; false when key holds no list: LPOS. Its lookup
// is a keyspace hit or a miss, counted here, after the command language has
// read the options, as Redis's lposCommand reads them before it looks
// (readsAfterArguments).
func (e *Engine) opLPos(key, element string, req lposRequest) (lposMatches, bool) {
	l, ok := e.listFor(key)
	e.noteLookup(ok)
	return lposMatches{l: l, element: element, req: req}, ok
}

// listMove is what an LMOVE asks for: the end of the source it pops from
// and the end of the destination it pushes to.
type listMove struct {
	fromFront, toFront bool
}

// listEnd reads LEFT or RIGHT, in any case, as Redis reads a list's end:
// true for LEFT, the head.
func listEnd(text string) (front bool, err error) {
	switch {
	case strings.EqualFold(text, "LEFT"):
		return true, nil
	case strings.EqualFold(text, "RIGHT"):
		return false, nil
	}
	return false, errSyntax
}

// endName is the end front names, as a record writes it.
func endName(front bool) string {
	if front {
		return "LEFT"
	}
	return "RIGHT"
}

// moveSource looks up the list an LMOVE, RPOPLPUSH or LMOVEM moves from and
// checks the type of what it moves to, in Redis's order: a source of another
// type is refused first, a source that holds nothing moves nothing (nil),
// and only then is a destination of another type refused, so that a missing
// source answers nil whatever the destination holds. The commands check
// their keys themselves (selfTypedCommands) for that reason.
//
// count is how many elements the move would take from the source, which an
// LMOVEM decides; the destination is checked only if it is above zero.
func (e *Engine) moveSource(src, dst string, count func(n int) int) (*data_structure.List, int, error) {
	l, ok := e.listFor(src)
	if !ok {
		if other, held := e.space.OwnerOf(src); held && other.KeyspaceName() != e.listStore.KeyspaceName() {
			return nil, 0, errWrongType
		}
		return nil, 0, nil
	}
	n := count(l.Len())
	if n == 0 {
		return l, 0, nil
	}
	if other, held := e.space.OwnerOf(dst); held && other.KeyspaceName() != e.listStore.KeyspaceName() {
		return nil, 0, errWrongType
	}
	return l, n, nil
}

// opLMove pops an element from l, the list src holds, at req's source end,
// pushes it onto the list dst holds at req's destination end, creating that
// list, and answers the element: LMOVE. src and dst may name one list, which
// rotates it. A source it empties is deleted. It is recorded as LMOVE with
// the ends named, as Redis propagates it.
func (e *Engine) opLMove(src string, l *data_structure.List, dst string, req listMove) string {
	value := e.moveOne(src, l, dst, req)
	e.aofRecord("LMOVE", src, dst, endName(req.fromFront), endName(req.toFront))
	return value
}

// opRPopLPush is LMOVE from the tail to the head, recorded as the RPOPLPUSH
// it is, as Redis propagates it.
func (e *Engine) opRPopLPush(src string, l *data_structure.List, dst string) string {
	value := e.moveOne(src, l, dst, listMove{toFront: true})
	e.aofRecord("RPOPLPUSH", src, dst)
	return value
}

// moveOne moves one element, LMOVE's and RPOPLPUSH's move.
func (e *Engine) moveOne(src string, l *data_structure.List, dst string, req listMove) string {
	var value string
	if req.fromFront {
		value, _ = l.PopFront()
	} else {
		value, _ = l.PopBack()
	}
	e.pushOnto(dst, req.toFront, value)
	if dst != src {
		e.dropListIfEmpty(src, l)
	}
	return value
}

// pushOnto pushes values onto the list key holds, creating it, each in turn
// at the head (front) or the tail. When key is the source of a move its list
// is still there, emptied or not, and the values go back onto it.
func (e *Engine) pushOnto(key string, front bool, values ...string) {
	d, ok := e.listFor(key)
	if !ok {
		d = data_structure.NewList()
		e.listStore.Put(key, d)
	}
	if front {
		d.PushFront(values...)
	} else {
		d.PushBack(values...)
	}
	e.listStore.Resize(key)
}

// lmovemRequest is what an LMOVEM asks for beyond LMOVE's ends.
type lmovemRequest struct {
	listMove
	// mode is how many it moves: one (lmovemOne), at most count (COUNT), or
	// exactly count or nothing (EXACTLY).
	mode  lmovemMode
	count int64
	// bulk keeps the moved block in the source's order (BULK); otherwise each
	// element is pushed as it is popped (OBO).
	bulk bool
}

type lmovemMode uint8

const (
	lmovemOne lmovemMode = iota
	lmovemUpTo
	lmovemExactly
)

// moves is how many elements the request moves from a source of n: Redis's
// lmovemGenericCommand.
func (r lmovemRequest) moves(n int) int {
	switch r.mode {
	case lmovemExactly:
		if int64(n) < r.count {
			return 0
		}
		return int(r.count)
	case lmovemUpTo:
		return int(min(r.count, int64(n)))
	}
	return min(1, n)
}

// reversed reports whether the moved block, in the order its elements were
// popped, reads backwards in the destination: pushed one by one at the head,
// or kept in the source's order when popped from its tail.
func (r lmovemRequest) reversed() bool {
	if r.bulk {
		return !r.fromFront
	}
	return r.toFront
}

// movedElement is the i-th element of the block an LMOVEM of n moves from l,
// in the order the block reads in the destination, which is the order its
// reply lists them in.
func (r lmovemRequest) movedElement(l *data_structure.List, n, i int) string {
	if r.reversed() {
		i = n - 1 - i
	}
	if !r.fromFront {
		i = l.Len() - 1 - i
	}
	v, _ := l.Index(i)
	return v
}

// opLMoveM moves n elements from l, the list src holds, to the list dst
// holds as req asks, creating it: LMOVEM. The block reads in the destination
// as movedElement orders it. src and dst may name one list, which rotates it.
// A source it empties is deleted. It is recorded as it was asked for, as
// Redis propagates it.
func (e *Engine) opLMoveM(src string, l *data_structure.List, dst string, req lmovemRequest, n int) {
	block := make([]string, n)
	for i := range block {
		block[i] = req.movedElement(l, n, i)
	}
	for range n {
		if req.fromFront {
			l.PopFront()
		} else {
			l.PopBack()
		}
	}
	if req.toFront {
		// A head push prepends, so the block goes on last element first.
		slices.Reverse(block)
	}
	e.pushOnto(dst, req.toFront, block...)
	if dst != src {
		e.dropListIfEmpty(src, l)
	}
	from, to := endName(req.fromFront), endName(req.toFront)
	switch req.mode {
	case lmovemOne:
		e.aofRecord("LMOVEM", src, dst, from, to)
	default:
		selector, order := "COUNT", "OBO"
		if req.mode == lmovemExactly {
			selector = "EXACTLY"
		}
		if req.bulk {
			order = "BULK"
		}
		e.aofRecord("LMOVEM", src, dst, from, to, selector, strconv.FormatInt(req.count, 10), order)
	}
}

// lmpopRequest is what an LMPOP asks for.
type lmpopRequest struct {
	keys  []string
	front bool
	count int64
}

// lmpopSource is the first of keys holding a list, which LMPOP pops from:
// Redis's mpopGenericCommand, which refuses a key of another type only when
// it reaches it, before any list after it. ok is false when none holds one.
func (e *Engine) lmpopSource(keys []string) (key string, l *data_structure.List, ok bool, err error) {
	for _, key := range keys {
		if l, ok := e.listFor(key); ok {
			return key, l, true, nil
		}
		if other, held := e.space.OwnerOf(key); held && other.KeyspaceName() != e.listStore.KeyspaceName() {
			return "", nil, false, errWrongType
		}
	}
	return "", nil, false, nil
}

// opLMPop pops up to req's count elements from l, the list key holds, at
// req's end, deleting the key if that empties it: LMPOP's pop, once its
// reply is admitted. It is recorded as the LPOP or RPOP with a count that
// replays it, the count it took, as Redis rewrites it (t_list.c,
// mpopGenericCommand).
func (e *Engine) opLMPop(key string, l *data_structure.List, front bool, count int64) {
	req := listPop{front: front, counted: true, count: min(count, int64(l.Len()))}
	e.opPop(key, l, req)
}
