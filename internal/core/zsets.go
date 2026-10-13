package core

import (
	"errors"
	"math"

	"github.com/brandopakel/keel/internal/data_structure"
)

// Typed operations: sorted sets and geo (phase 4 of docs/embedding-plan.md;
// the family recipe is docs/phase4-family-recipe.md).
//
// Redis has no empty sorted set: removing the last member removes the key,
// and a ZADD that adds nothing to a key that did not exist leaves no key
// behind (zsetSettle). Each write stages the record develop logged for it,
// byte for byte: ZADD and GEOADD as the client sent them (the request's
// text; a Go caller, which has none, gets a ZADD formatted from the values),
// ZREM with its members as given, ZINCRBY as the ZADD of the score it made,
// ZPOPMIN and ZPOPMAX as the ZREM of what they popped. A ZADD or ZREM that
// changes nothing is logged although Redis does not log it, and GEOADD as
// itself where Redis propagates a ZADD of the geohash scores; the family's
// missing-commands step moves both to Redis's form.

// errZIncrNaN refuses a ZINCRBY whose sum is not a number, in Redis's words.
var errZIncrNaN = errors.New("ERR resulting score is not a number (NaN)")

// zsetFor is the sorted set key holds, and false for none.
func (e *Engine) zsetFor(key string) (*data_structure.ZSet, bool) {
	return e.zsetStore.Get(key)
}

// zsetSettle records that a sorted set was changed in place: it is dropped when
// empty and re-measured otherwise.
func (e *Engine) zsetSettle(key string, zs *data_structure.ZSet) {
	if zs.Len() == 0 {
		e.zsetStore.Delete(key)
		return
	}
	e.zsetStore.Resize(key)
}

// zaddRequest is what a ZADD or a GEOADD asks for: its flags (NX, XX), the
// scores and members, and text, the client's arguments after the command's
// name, which the record repeats; nil for a Go caller.
type zaddRequest struct {
	flags   int
	scores  []float64
	members []string
	text    []string
}

// zaddApply adds every score/member pair to the set at key under flags, and
// reports how many members were new and how many were new or rescored.
//
// The set is created on demand, except under XX, where nothing new may be added
// and so a key that does not exist stays that way.
func (e *Engine) zaddApply(key string, scores []float64, members []string, flags int) (added, changed int) {
	zs, ok := e.zsetFor(key)
	if !ok {
		if flags&data_structure.ZAddXX != 0 {
			return 0, 0
		}
		zs = data_structure.CreateZSet()
		e.zsetStore.Put(key, zs)
	}
	for i, score := range scores {
		switch zs.Add(score, members[i], flags) {
		case data_structure.ZAddAdded:
			added++
			changed++
		case data_structure.ZAddUpdated:
			changed++
		}
	}
	e.zsetSettle(key, zs)
	return added, changed
}

// opZAdd adds the request's pairs to the sorted set key holds, as ZADD does,
// and answers how many members were new and how many were new or rescored.
// It is recorded as the ZADD the client sent, or one formatted from the
// values for a Go caller.
func (e *Engine) opZAdd(key string, req zaddRequest) (added, changed int64) {
	a, c := e.zaddApply(key, req.scores, req.members, req.flags)
	if req.text != nil {
		e.aofRecordArgs("ZADD", req.text)
	} else {
		e.aofRecordArgs("ZADD", zaddText(key, req))
	}
	return int64(a), int64(c)
}

// zaddText is a Go caller's ZADD as a client would send it.
func zaddText(key string, req zaddRequest) []string {
	text := make([]string, 0, 2+2*len(req.members))
	text = append(text, key)
	switch {
	case req.flags&data_structure.ZAddNX != 0:
		text = append(text, "NX")
	case req.flags&data_structure.ZAddXX != 0:
		text = append(text, "XX")
	}
	for i, member := range req.members {
		text = append(text, formatZScore(req.scores[i]), member)
	}
	return text
}

// opGeoAdd adds the request's geohash scores and members to the sorted set
// key holds, as GEOADD does. It is recorded as the GEOADD the client sent
// (see above); a Go caller's is recorded as the ZADD of the scores, as Redis
// propagates GEOADD.
func (e *Engine) opGeoAdd(key string, req zaddRequest) (added, changed int64) {
	a, c := e.zaddApply(key, req.scores, req.members, req.flags)
	if req.text != nil {
		e.aofRecordArgs("GEOADD", req.text)
	} else {
		e.aofRecordArgs("ZADD", zaddText(key, req))
	}
	return int64(a), int64(c)
}

// opZRem removes each of members from the sorted set key holds, removing the
// key with its last member, and answers how many it removed. It is recorded
// as ZREM whatever it removed, as develop logs it (see above).
func (e *Engine) opZRem(key string, members []string) int64 {
	e.aofRecordKeyArgs("ZREM", key, members)
	zs, ok := e.zsetFor(key)
	if !ok {
		return 0
	}
	removed := int64(0)
	for _, member := range members {
		if zs.Remove(member) {
			removed++
		}
	}
	e.zsetSettle(key, zs)
	return removed
}

// opZScore is member's score in the sorted set key holds, and false for none.
func (e *Engine) opZScore(key, member string) (float64, bool) {
	zs, ok := e.zsetFor(key)
	if !ok {
		return 0, false
	}
	return zs.Score(member)
}

// opZCard counts the members of the sorted set key holds.
func (e *Engine) opZCard(key string) int64 {
	zs, ok := e.zsetFor(key)
	if !ok {
		return 0
	}
	return int64(zs.Len())
}

// opZIncrBy adds increment to member's score in the sorted set key holds, a
// missing member or key being 0, and answers the score it made. A sum that is
// not a number is refused. It is recorded as the ZADD of that score, as
// develop logs it, readable by every older build.
func (e *Engine) opZIncrBy(key string, increment float64, member string) (float64, error) {
	score := increment
	if zs, ok := e.zsetFor(key); ok {
		if old, present := zs.Score(member); present {
			score += old
		}
	}
	if math.IsNaN(score) {
		return 0, errZIncrNaN
	}
	e.zaddApply(key, []float64{score}, []string{member}, 0)
	e.aofRecord("ZADD", key, formatZScore(score), member)
	return score, nil
}
