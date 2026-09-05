package data_structure

import "math"

// ZSet is a sorted set: each member carries a float score, and the members are
// ordered by score, then by member for equal scores.
//
// Two structures hold the same members. The map answers "what is this member's
// score" in one step, which every command needs first; the skip list keeps the
// order, which is what ranks and range queries walk. Redis pairs a dict with a
// skip list for the same reason.
type ZSet struct {
	dict map[string]float64
	sl   *skiplist
	// memberBytes tracks the total length of members held, so MemUsage is O(1)
	// rather than a walk of the set.
	memberBytes uint64
}

// Flags for Add. Without either, a member is added if new and rescored if not.
const (
	// ZAddNX adds new members only; a member already present is left alone.
	ZAddNX = 1 << iota
	// ZAddXX rescores existing members only; a new member is not added.
	ZAddXX
)

// ZAddResult says what Add did with a member.
type ZAddResult uint8

const (
	// ZAddNop means nothing changed: a flag ruled the member out, or it was
	// already there with the same score.
	ZAddNop ZAddResult = iota
	// ZAddAdded means the member was new.
	ZAddAdded
	// ZAddUpdated means the member was present and its score changed.
	ZAddUpdated
)

func CreateZSet() *ZSet {
	return &ZSet{dict: make(map[string]float64), sl: newSkiplist()}
}

// Add puts member in the set at score, subject to flags, and reports what
// happened.
//
// NaN is not a score: it compares false against everything, so a member
// stored under it could never be found again by the skip list, and the next
// rescore would fail an assertion there. Callers refuse it with an error of
// their own; this is the guard behind them, answering that nothing was done.
func (zs *ZSet) Add(score float64, member string, flags int) ZAddResult {
	if math.IsNaN(score) {
		return ZAddNop
	}
	current, present := zs.dict[member]
	if present {
		if flags&ZAddNX != 0 || current == score {
			return ZAddNop
		}
		zs.sl.updateScore(current, member, score)
		zs.dict[member] = score
		return ZAddUpdated
	}
	if flags&ZAddXX != 0 {
		return ZAddNop
	}
	zs.sl.insert(score, member)
	zs.dict[member] = score
	zs.memberBytes += uint64(len(member))
	return ZAddAdded
}

// Remove takes member out and reports whether it was there.
func (zs *ZSet) Remove(member string) bool {
	score, present := zs.dict[member]
	if !present {
		return false
	}
	delete(zs.dict, member)
	zs.sl.remove(score, member)
	zs.memberBytes -= uint64(len(member))
	return true
}

// Score returns a member's score and whether the member is present.
func (zs *ZSet) Score(member string) (float64, bool) {
	score, present := zs.dict[member]
	return score, present
}

// Rank returns a member's 0-based position, counted from the lowest score, or
// from the highest when reverse is set. The second result is false for a
// member that is not there.
func (zs *ZSet) Rank(member string, reverse bool) (int64, bool) {
	score, present := zs.dict[member]
	if !present {
		return 0, false
	}
	// The skip list counts from one.
	rank := int64(zs.sl.rank(score, member)) - 1
	if reverse {
		rank = int64(zs.sl.length) - 1 - rank
	}
	return rank, true
}

// Len is the number of members.
func (zs *ZSet) Len() int { return len(zs.dict) }

// MemUsage estimates the bytes held, in O(1).
//
// A sorted set pays for a member twice: once in the score dictionary and once
// in the skiplist node, which carries the element, the score, a back pointer
// and a level slice. Measured over 200,000 members that comes to 155 bytes for
// a 20-byte member, so 135 bytes of structure per member.
func (zs *ZSet) MemUsage() uint64 {
	return zsetBaseBytes + uint64(len(zs.dict))*zsetMemberOverhead + zs.memberBytes
}

// RangeByRank returns a bounded window in O(log(n)+returned elements).
func (zs *ZSet) RangeByRank(start, stop int, reverse bool) ([]string, []float64) {
	n := zs.Len()
	if start < 0 {
		start += n
	}
	if stop < 0 {
		stop += n
	}
	if start < 0 {
		start = 0
	}
	if stop >= n {
		stop = n - 1
	}
	if start > stop || start >= n {
		return nil, nil
	}
	rank := start + 1
	if reverse {
		rank = n - start
	}
	node := zs.sl.nodeAtRank(uint64(rank))
	members := make([]string, 0, stop-start+1)
	scores := make([]float64, 0, stop-start+1)
	for i := start; i <= stop && node != nil; i++ {
		members = append(members, node.ele)
		scores = append(scores, node.score)
		if reverse {
			node = node.backward
		} else {
			node = node.level[0].forward
		}
	}
	return members, scores
}
