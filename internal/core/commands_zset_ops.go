package core

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/brandopakel/keel/internal/data_structure"
)

// The sorted-set algebra: ZUNION, ZINTER and ZDIFF, their STORE forms, and
// ZINTERCARD, as Redis's zunionInterDiffGenericCommand computes them
// (t_zset.c, 2885-3170). A source may be a sorted set or a set, whose members
// score 1; a missing key is empty.

type zsetOp uint8

const (
	zsetUnion zsetOp = iota
	zsetInter
	zsetDiff
)

type zsetAggregate uint8

const (
	aggregateSum zsetAggregate = iota
	aggregateMin
	aggregateMax
	// aggregateCount is Redis 8's COUNT: each member counts its source's
	// weight, whatever its score.
	aggregateCount
)

var (
	errWeightNotFloat = errors.New("ERR weight value is not a float")
	errLimitNegative  = errors.New("ERR LIMIT can't be negative")
)

// zsetOpSource is one input: a sorted set, a set, or nothing.
type zsetOpSource struct {
	zs     *data_structure.ZSet
	set    *data_structure.Set
	weight float64
}

func (s zsetOpSource) length() int {
	switch {
	case s.zs != nil:
		return s.zs.Len()
	case s.set != nil:
		return s.set.Len()
	}
	return 0
}

// visit walks the source's members with their scores, a set's at 1.
func (s zsetOpSource) visit(yield func(string, float64) bool) {
	switch {
	case s.zs != nil:
		s.zs.VisitRangeByRank(0, -1, false, yield)
	case s.set != nil:
		for i := 0; i < s.set.Len(); i++ {
			member, _ := s.set.MemberAt(i)
			if !yield(member, 1) {
				return
			}
		}
	}
}

// find is member's score in the source, and false for none.
func (s zsetOpSource) find(member string) (float64, bool) {
	switch {
	case s.zs != nil:
		return s.zs.Score(member)
	case s.set != nil:
		return 1, s.set.Contains(member)
	}
	return 0, false
}

// zsetOpRequest is what the arguments ask for.
type zsetOpRequest struct {
	sources    []zsetOpSource
	aggregate  zsetAggregate
	withScores bool
	limit      int64
}

// zsetOpArguments reads numkeys, looks each key up, then reads the options,
// in Redis's order: a key of another type is refused before a malformed
// option, and every key up to it counts as a keyspace hit or a miss. name is
// Redis's lower-case full name, which its refusal of numkeys 0 quotes.
func (e *Engine) zsetOpArguments(name string, args []string, op zsetOp, store, cardinality bool) (req zsetOpRequest, err error) {
	n, valid := e.counterInteger(args[0])
	if !valid {
		return req, errNotAnInteger
	}
	if n < 1 {
		return req, fmt.Errorf("ERR at least 1 input key is needed for '%s' command", name)
	}
	if n > int64(len(args)-1) {
		return req, errSyntax
	}
	req.sources = make([]zsetOpSource, n)
	for i, key := range args[1 : 1+n] {
		req.sources[i].weight = 1
		owner, held := e.space.OwnerOf(key)
		e.noteLookup(held)
		if !held {
			continue
		}
		switch owner.KeyspaceName() {
		case e.zsetStore.KeyspaceName():
			req.sources[i].zs, _ = e.zsetFor(key)
		case e.setStore.KeyspaceName():
			req.sources[i].set, _ = e.setFor(key)
		default:
			return req, errWrongType
		}
	}
	for j := int(1 + n); j < len(args); {
		remaining := len(args) - j
		switch opt := strings.ToUpper(args[j]); {
		case op != zsetDiff && !cardinality && remaining >= int(n)+1 && opt == "WEIGHTS":
			j++
			for i := range req.sources {
				w, err := parseWeight(args[j])
				if err != nil {
					return req, err
				}
				req.sources[i].weight = w
				j++
			}
		case op != zsetDiff && !cardinality && remaining >= 2 && opt == "AGGREGATE":
			switch strings.ToUpper(args[j+1]) {
			case "SUM":
				req.aggregate = aggregateSum
			case "MIN":
				req.aggregate = aggregateMin
			case "MAX":
				req.aggregate = aggregateMax
			case "COUNT":
				req.aggregate = aggregateCount
			default:
				return req, errSyntax
			}
			j += 2
		case !store && !cardinality && opt == "WITHSCORES":
			req.withScores = true
			j++
		case cardinality && remaining >= 2 && opt == "LIMIT":
			// getPositiveLongFromObjectOrReply answers a value that is not an
			// integer in the words it was given for a negative one.
			l, valid := e.counterInteger(args[j+1])
			if !valid || l < 0 {
				return req, errLimitNegative
			}
			req.limit = l
			j += 2
		default:
			return req, errSyntax
		}
	}
	return req, nil
}

// parseWeight reads a weight as getDoubleFromObject does: any float but NaN.
func parseWeight(text string) (float64, error) {
	w, err := parseZScore(text)
	if err != nil {
		return 0, errWeightNotFloat
	}
	return w, nil
}

func weighted(score, weight float64, aggregate zsetAggregate) float64 {
	if aggregate == aggregateCount {
		return weight
	}
	return weight * score
}

// aggregateInto combines val into target as zunionInterAggregate does: a sum
// of opposite infinities is 0.
func aggregateInto(target, val float64, aggregate zsetAggregate) float64 {
	switch aggregate {
	case aggregateMin:
		if val < target {
			return val
		}
		return target
	case aggregateMax:
		if val > target {
			return val
		}
		return target
	}
	if sum := target + val; !math.IsNaN(sum) {
		return sum
	}
	return 0
}

// zsetOpCompute runs op over req's sources and answers the result as a
// sorted set, or for a cardinality the count alone.
func zsetOpCompute(op zsetOp, req zsetOpRequest, cardinality bool) (*data_structure.ZSet, int64) {
	sources := req.sources
	if op != zsetDiff {
		// Smallest first, as Redis orders them: an intersection walks the
		// smallest, and a union aggregates in this order.
		sources = append([]zsetOpSource(nil), sources...)
		sort.SliceStable(sources, func(i, j int) bool { return sources[i].length() < sources[j].length() })
	}
	result := data_structure.CreateZSet()
	var count int64
	switch op {
	case zsetInter:
		if sources[0].length() == 0 {
			break
		}
		sources[0].visit(func(member string, score float64) bool {
			total := weighted(score, sources[0].weight, req.aggregate)
			if math.IsNaN(total) {
				total = 0
			}
			for _, src := range sources[1:] {
				value, present := src.find(member)
				if !present {
					return true
				}
				total = aggregateInto(total, weighted(value, src.weight, req.aggregate), req.aggregate)
			}
			if cardinality {
				count++
				return req.limit == 0 || count < req.limit
			}
			result.Add(total, member, 0)
			return true
		})
	case zsetUnion:
		scores := map[string]float64{}
		var order []string
		for _, src := range sources {
			src.visit(func(member string, score float64) bool {
				value := weighted(score, src.weight, req.aggregate)
				if math.IsNaN(value) {
					value = 0
				}
				if current, present := scores[member]; present {
					scores[member] = aggregateInto(current, value, req.aggregate)
				} else {
					scores[member] = value
					order = append(order, member)
				}
				return true
			})
		}
		for _, member := range order {
			result.Add(scores[member], member, 0)
		}
	case zsetDiff:
		sources[0].visit(func(member string, score float64) bool {
			for _, src := range sources[1:] {
				if _, present := src.find(member); present {
					return true
				}
			}
			result.Add(score, member, 0)
			return true
		})
	}
	return result, count
}

func (e *Engine) cmdZUNION(args []string) []byte { return e.zsetOpRead("zunion", args, zsetUnion) }
func (e *Engine) cmdZINTER(args []string) []byte { return e.zsetOpRead("zinter", args, zsetInter) }
func (e *Engine) cmdZDIFF(args []string) []byte  { return e.zsetOpRead("zdiff", args, zsetDiff) }

// zsetOpRead answers ZUNION, ZINTER or ZDIFF: the members, with WITHSCORES
// each followed by its score, in score order.
func (e *Engine) zsetOpRead(name string, args []string, op zsetOp) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments(strings.ToUpper(name)), false)
	}
	req, err := e.zsetOpArguments(name, args, op, false, false)
	if err != nil {
		return e.encode(err, false)
	}
	result, _ := zsetOpCompute(op, req, false)
	walk := func(yield func(string, float64) bool) { result.VisitRangeByRank(0, -1, false, yield) }
	if req.withScores && e.replyRESP3 {
		return e.scoredReply3(walk, true)
	}
	return e.scoredReply(walk, req.withScores)
}

func (e *Engine) cmdZUNIONSTORE(args []string) []byte {
	return e.zsetOpStore("ZUNIONSTORE", args, zsetUnion)
}
func (e *Engine) cmdZINTERSTORE(args []string) []byte {
	return e.zsetOpStore("ZINTERSTORE", args, zsetInter)
}
func (e *Engine) cmdZDIFFSTORE(args []string) []byte {
	return e.zsetOpStore("ZDIFFSTORE", args, zsetDiff)
}

// zsetOpStore stores the result at the destination, in place of whatever it
// held, deleting it for an empty result, and answers the result's size. It
// is recorded as sent when the destination changed, as Redis propagates it.
func (e *Engine) zsetOpStore(name string, args []string, op zsetOp) []byte {
	e.recordsItself()
	if len(args) < 3 {
		return e.encode(wrongArguments(name), false)
	}
	req, err := e.zsetOpArguments(strings.ToLower(name), args[1:], op, true, false)
	if err != nil {
		return e.encode(err, false)
	}
	result, _ := zsetOpCompute(op, req, false)
	members := make([]string, 0, result.Len())
	scores := make([]float64, 0, result.Len())
	result.VisitRangeByRank(0, -1, false, func(member string, score float64) bool {
		members, scores = append(members, member), append(scores, score)
		return true
	})
	if e.opZStore(args[0], members, scores) {
		e.aofRecordArgs(name, args)
	}
	reply := e.reply()
	return reply.Int(int64(len(members))).Bytes()
}

// cmdZINTERCARD counts the intersection, stopping at LIMIT when one is given.
func (e *Engine) cmdZINTERCARD(args []string) []byte {
	if len(args) < 2 {
		return e.encode(wrongArguments("ZINTERCARD"), false)
	}
	req, err := e.zsetOpArguments("zintercard", args, zsetInter, false, true)
	if err != nil {
		return e.encode(err, false)
	}
	_, count := zsetOpCompute(zsetInter, req, true)
	reply := e.reply()
	return reply.Int(count).Bytes()
}
