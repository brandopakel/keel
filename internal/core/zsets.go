package core

import (
	"errors"
	"strconv"

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

// zaddRequest is what a ZADD or a GEOADD asks for: its flags (NX, XX, GT, LT,
// INCR), the scores and members, and text, the client's arguments after the
// command's name, which a ZADD's record repeats; nil for a Go caller. A
// GEOADD's record is the ZADD Redis rewrites it to, its options as the
// client gave them (options) and its geohash scores.
type zaddRequest struct {
	flags   int
	scores  []float64
	members []string
	text    []string
	options []string
}

// zaddResult is what a ZADD did: how many members it added, how many it
// added or rescored, and for INCR the score it made and whether it made one
// (an NX, XX, GT or LT that ruled the member out did not).
type zaddResult struct {
	added, changed int64
	score          float64
	processed      bool
}

// zaddApply adds every score/member pair to the set at key under flags, as
// Redis's zaddGenericCommand does. The set is created on demand, except under
// XX, where nothing new may be added and so a key that does not exist stays
// that way. An INCR whose sum is not a number is refused, after the pairs
// before it.
//
// changedPairs, when not nil, collects the score and member of each member
// added or rescored, at the score it now has: a Go caller's record.
func (e *Engine) zaddApply(key string, req zaddRequest, changedPairs *[]string) (zaddResult, error) {
	var res zaddResult
	zs, ok := e.zsetFor(key)
	if !ok {
		if req.flags&data_structure.ZAddXX != 0 {
			return res, nil
		}
		zs = data_structure.CreateZSet()
		e.zsetStore.Put(key, zs)
	}
	var err error
	for i, score := range req.scores {
		outcome, newScore, nan := zs.AddScore(score, req.members[i], req.flags)
		if nan {
			err = errZIncrNaN
			break
		}
		switch outcome {
		case data_structure.ZAddAdded:
			res.added++
			res.changed++
		case data_structure.ZAddUpdated:
			res.changed++
		}
		if outcome != data_structure.ZAddNop {
			res.processed, res.score = true, newScore
		}
		if changedPairs != nil && (outcome == data_structure.ZAddAdded || outcome == data_structure.ZAddUpdated) {
			*changedPairs = append(*changedPairs, formatZScore(newScore), req.members[i])
		}
	}
	e.zsetSettle(key, zs)
	return res, err
}

// opZAdd adds the request's pairs to the sorted set key holds, as ZADD does.
// It is recorded only when it added or rescored a member, as Redis counts
// server.dirty (t_zset.c, zaddGenericCommand, line 2075): as the ZADD the
// client sent, or for a Go caller as a ZADD of what changed at the scores it
// now has.
func (e *Engine) opZAdd(key string, req zaddRequest) (zaddResult, error) {
	var pairs *[]string
	if req.text == nil {
		pairs = new([]string)
	}
	res, err := e.zaddApply(key, req, pairs)
	if res.changed > 0 {
		if req.text != nil {
			e.aofRecordArgs("ZADD", req.text)
		} else {
			e.aofRecordKeyArgs("ZADD", key, *pairs)
		}
	}
	return res, err
}

// opGeoAdd adds the request's geohash scores and members to the sorted set
// key holds, as GEOADD does. It is recorded, when it added or moved a member,
// as the ZADD Redis rewrites GEOADD to (geo.c, geoaddCommand, lines
// 490-503): the options as given, then each score as the 52-bit integer it
// is, and the member.
func (e *Engine) opGeoAdd(key string, req zaddRequest) (zaddResult, error) {
	res, err := e.zaddApply(key, req, nil)
	if res.changed > 0 {
		record := make([]string, 0, len(req.options)+2*len(req.members))
		record = append(record, req.options...)
		for i, member := range req.members {
			record = append(record, strconv.FormatInt(int64(req.scores[i]), 10), member)
		}
		e.aofRecordKeyArgs("ZADD", key, record)
	}
	return res, err
}

// opZRem removes each of members from the sorted set key holds, removing the
// key with its last member, and answers how many it removed. It is recorded,
// as ZREM with its members as given, only when it removed one (t_zset.c,
// zremCommand, line 2148).
func (e *Engine) opZRem(key string, members []string) int64 {
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
	if removed > 0 {
		e.aofRecordKeyArgs("ZREM", key, members)
	}
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
// not a number is refused. It is recorded, when it changed the score, as the
// ZADD of the score it made, as develop logs it, readable by every older
// build; an increment that leaves the score as it was is not, as Redis
// counts it (zaddGenericCommand, line 2075).
func (e *Engine) opZIncrBy(key string, increment float64, member string) (float64, error) {
	res, err := e.zaddApply(key, zaddRequest{flags: data_structure.ZAddIncr,
		scores: []float64{increment}, members: []string{member}}, nil)
	if err != nil {
		return 0, err
	}
	if res.changed > 0 {
		e.aofRecord("ZADD", key, formatZScore(res.score), member)
	}
	return res.score, nil
}

// opZRemRange removes the members of the sorted set key holds that z's range
// takes in, removing the key with its last member, and answers how many it
// removed. It is recorded, as the command the client sent (name and text),
// only when it removed one (t_zset.c, zremrangeGenericCommand, line 2280); a
// Go caller, with no text, is recorded as the ZREM of what it removed.
func (e *Engine) opZRemRange(key string, z zrangeArguments, name string, text []string) int64 {
	zs, ok := e.zsetFor(key)
	if !ok {
		return 0
	}
	var doomed []string
	zrangeWalk(zs, z)(func(member string, _ float64) bool {
		doomed = append(doomed, member)
		return true
	})
	for _, member := range doomed {
		zs.Remove(member)
	}
	e.zsetSettle(key, zs)
	if len(doomed) > 0 {
		if text != nil {
			e.aofRecordArgs(name, text)
		} else {
			e.aofRecordKeyArgs("ZREM", key, doomed)
		}
	}
	return int64(len(doomed))
}

// opZStore makes dst the sorted set of members at scores, replacing whatever
// dst held, of any type, as Redis's ZRANGESTORE and ZUNIONSTORE do; with no
// members it deletes dst. It reports whether dst changed - stored, or
// deleted where it existed - which is when Redis counts the command as a
// change (t_zset.c, 3150-3162 and 3360-3371). The caller records the command.
func (e *Engine) opZStore(dst string, members []string, scores []float64) bool {
	existed := e.space.DeleteAnywhere(dst)
	if len(members) == 0 {
		return existed
	}
	zs := data_structure.CreateZSet()
	for i, member := range members {
		zs.Add(scores[i], member, 0)
	}
	e.zsetStore.Put(dst, zs)
	return true
}
