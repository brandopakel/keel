/*
 * Copyright (c) 2009-2012, Salvatore Sanfilippo <antirez at gmail dot com>
 * Copyright (c) 2009-2012, Pieter Noordhuis <pcnoordhuis at gmail dot com>
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are met:
 *
 *   * Redistributions of source code must retain the above copyright notice,
 *     this list of conditions and the following disclaimer.
 *   * Redistributions in binary form must reproduce the above copyright
 *     notice, this list of conditions and the following disclaimer in the
 *     documentation and/or other materials provided with the distribution.
 *   * Neither the name of Redis nor the names of its contributors may be used
 *     to endorse or promote products derived from this software without
 *     specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
 * AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
 * IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE
 * ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT OWNER OR CONTRIBUTORS BE
 * LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR
 * CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF
 * SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS
 * INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN
 * CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE)
 * ARISING IN ANY WAY OUT OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE
 * POSSIBILITY OF SUCH DAMAGE.
 */

// This file is a Go translation of the zskiplist in Redis 7.2.4's
// src/t_zset.c, which carries the licence above. The translation was made for
// keel by Brando Pakel in 2026 and is distributed under the same terms.

package data_structure

// The skip list under a sorted set.
//
// Members are kept ordered by score and then by member, which is the order
// every range query walks. Each node carries a slice of forward pointers, one
// per level, and with each pointer the number of nodes it skips - the span -
// so a rank is the sum of the spans crossed on the way down to a node rather
// than a count of the nodes before it.
//
// The list does not know which members it holds beyond what is reachable
// through those pointers: the sorted set pairs it with a map from member to
// score, and looks up the score there before asking the list for anything,
// since every operation here needs both to find its node.

const (
	skiplistMaxLevel = 32
	// skiplistP is the probability that a node reaches the next level up. A
	// quarter gives the list about the shape of a 4-way tree.
	skiplistP = 0.25
)

// skiplistLevelThreshold is skiplistP scaled to the 32 random bits a level
// draw consumes: a draw below it promotes the node one level.
const skiplistLevelThreshold = uint64(skiplistP * (1 << 32))

// skiplistSeed starts every list's level generator. A fixed seed, because the
// levels a list happens to choose do not affect anything it answers, and a
// generator of its own keeps the hot path off the lock inside math/rand.
const skiplistSeed = 0x9e3779b97f4a7c15

type skiplistLevel struct {
	forward *skiplistNode
	// span is how many nodes forward is ahead of this one at this level.
	span uint64
}

type skiplistNode struct {
	ele      string
	score    float64
	backward *skiplistNode
	level    []skiplistLevel
}

type skiplist struct {
	// header is a sentinel carrying every level; the first real node is
	// header.level[0].forward.
	header *skiplistNode
	tail   *skiplistNode
	length uint64
	// level is the highest level any node currently occupies.
	level int
	rng   uint64
}

// rangeSpec is an interval of scores, each end open or closed.
type rangeSpec struct {
	min, max     float64
	minEx, maxEx bool
}

func (r rangeSpec) gteMin(value float64) bool {
	if r.minEx {
		return value > r.min
	}
	return value >= r.min
}

func (r rangeSpec) lteMax(value float64) bool {
	if r.maxEx {
		return value < r.max
	}
	return value <= r.max
}

func newSkiplistNode(level int, score float64, ele string) *skiplistNode {
	return &skiplistNode{
		ele:   ele,
		score: score,
		level: make([]skiplistLevel, level),
	}
}

func newSkiplist() *skiplist {
	return &skiplist{
		header: newSkiplistNode(skiplistMaxLevel, 0, ""),
		level:  1,
		rng:    skiplistSeed,
	}
}

// first returns the lowest-ranked node, or nil when the list is empty.
func (sl *skiplist) first() *skiplistNode { return sl.header.level[0].forward }

// next returns the node after n in rank order.
func (n *skiplistNode) next() *skiplistNode { return n.level[0].forward }

// randomBits is an xorshift64* step, which is plenty for choosing levels.
func (sl *skiplist) randomBits() uint64 {
	x := sl.rng
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	sl.rng = x
	return x * 2685821657736338717
}

// randomLevel draws the level for a new node: between 1 and skiplistMaxLevel,
// with each higher level a quarter as likely as the one below it.
func (sl *skiplist) randomLevel() int {
	level := 1
	for sl.randomBits()>>32 < skiplistLevelThreshold {
		level++
	}
	if level > skiplistMaxLevel {
		return skiplistMaxLevel
	}
	return level
}

// sortsBefore reports whether node n comes before (score, ele) in list order:
// by score, then by member for equal scores.
func (n *skiplistNode) sortsBefore(score float64, ele string) bool {
	return n.score < score || (n.score == score && n.ele < ele)
}

// insert adds (score, ele), which the caller guarantees is not already
// present, and returns its node.
func (sl *skiplist) insert(score float64, ele string) *skiplistNode {
	var update [skiplistMaxLevel]*skiplistNode
	var rank [skiplistMaxLevel]uint64

	// Walk down from the top level, recording at each level the last node
	// before the insertion point and the rank reached on the way to it.
	x := sl.header
	for i := sl.level - 1; i >= 0; i-- {
		if i != sl.level-1 {
			rank[i] = rank[i+1]
		}
		for f := x.level[i].forward; f != nil && f.sortsBefore(score, ele); f = x.level[i].forward {
			rank[i] += x.level[i].span
			x = f
		}
		update[i] = x
	}

	level := sl.randomLevel()
	if level > sl.level {
		// The new node is taller than the list. The header takes over the
		// levels it never had, each spanning the whole list.
		for i := sl.level; i < level; i++ {
			rank[i] = 0
			update[i] = sl.header
			update[i].level[i].span = sl.length
		}
		sl.level = level
	}

	x = newSkiplistNode(level, score, ele)
	for i := 0; i < level; i++ {
		x.level[i].forward = update[i].level[i].forward
		update[i].level[i].forward = x

		// Split the span update[i] used to cover around the new node.
		x.level[i].span = update[i].level[i].span - (rank[0] - rank[i])
		update[i].level[i].span = (rank[0] - rank[i]) + 1
	}
	// Levels the new node does not reach still have one more node beneath
	// them.
	for i := level; i < sl.level; i++ {
		update[i].level[i].span++
	}

	if update[0] != sl.header {
		x.backward = update[0]
	}
	if x.level[0].forward != nil {
		x.level[0].forward.backward = x
	} else {
		sl.tail = x
	}
	sl.length++
	return x
}

// unlink removes x, given the update vector a search for it produced.
func (sl *skiplist) unlink(x *skiplistNode, update *[skiplistMaxLevel]*skiplistNode) {
	for i := 0; i < sl.level; i++ {
		if update[i].level[i].forward == x {
			update[i].level[i].span += x.level[i].span - 1
			update[i].level[i].forward = x.level[i].forward
		} else {
			update[i].level[i].span--
		}
	}
	if x.level[0].forward != nil {
		x.level[0].forward.backward = x.backward
	} else {
		sl.tail = x.backward
	}
	// Drop levels that no longer have a node in them.
	for sl.level > 1 && sl.header.level[sl.level-1].forward == nil {
		sl.level--
	}
	sl.length--
}

// seek finds the node for (score, ele), filling update with the last node
// before it at every level. It returns nil when there is no such node.
func (sl *skiplist) seek(score float64, ele string, update *[skiplistMaxLevel]*skiplistNode) *skiplistNode {
	x := sl.header
	for i := sl.level - 1; i >= 0; i-- {
		for f := x.level[i].forward; f != nil && f.sortsBefore(score, ele); f = x.level[i].forward {
			x = f
		}
		update[i] = x
	}
	// Several members can share a score, so the candidate has to match on
	// both.
	x = x.level[0].forward
	if x != nil && x.score == score && x.ele == ele {
		return x
	}
	return nil
}

// remove deletes (score, ele) and reports whether it was there.
func (sl *skiplist) remove(score float64, ele string) bool {
	var update [skiplistMaxLevel]*skiplistNode
	x := sl.seek(score, ele, &update)
	if x == nil {
		return false
	}
	sl.unlink(x, &update)
	return true
}

// updateScore moves ele from curScore to newScore and returns its node. The
// member must be present at curScore.
//
// When the new score keeps the node between its neighbours it is changed in
// place; otherwise the node is unlinked and a fresh one inserted where the new
// score belongs.
func (sl *skiplist) updateScore(curScore float64, ele string, newScore float64) *skiplistNode {
	var update [skiplistMaxLevel]*skiplistNode
	x := sl.seek(curScore, ele, &update)
	if x == nil {
		panic("skiplist: updateScore on a member that is not present")
	}

	prev, next := x.backward, x.level[0].forward
	if (prev == nil || prev.score < newScore) && (next == nil || next.score > newScore) {
		x.score = newScore
		return x
	}

	sl.unlink(x, &update)
	return sl.insert(newScore, ele)
}

// rank returns the 1-based position of (score, ele), or 0 if it is absent.
func (sl *skiplist) rank(score float64, ele string) uint64 {
	var traversed uint64
	x := sl.header
	for i := sl.level - 1; i >= 0; i-- {
		for f := x.level[i].forward; f != nil && (f.score < score || (f.score == score && f.ele <= ele)); f = x.level[i].forward {
			traversed += x.level[i].span
			x = f
		}
		// x is the header until something has been crossed, and the header
		// holds no member.
		if x != sl.header && x.score == score && x.ele == ele {
			return traversed
		}
	}
	return 0
}

// nodeAtRank returns the node at a 1-based rank, or nil when the rank is out of
// range.
func (sl *skiplist) nodeAtRank(rank uint64) *skiplistNode {
	if rank == 0 {
		return nil
	}
	var traversed uint64
	x := sl.header
	for i := sl.level - 1; i >= 0; i-- {
		for f := x.level[i].forward; f != nil && traversed+x.level[i].span <= rank; f = x.level[i].forward {
			traversed += x.level[i].span
			x = f
		}
		if traversed == rank {
			return x
		}
	}
	return nil
}

// overlaps is the cheap test for a range that cannot hold anything: empty by
// construction, or entirely above or below every score held. It can still
// answer true for a range that falls between two scores; firstInRange and
// lastInRange settle that.
func (sl *skiplist) overlaps(r rangeSpec) bool {
	// A range that can hold nothing is answered without looking.
	if r.min > r.max || (r.min == r.max && (r.minEx || r.maxEx)) {
		return false
	}
	if sl.tail == nil || !r.gteMin(sl.tail.score) {
		return false
	}
	if head := sl.first(); head == nil || !r.lteMax(head.score) {
		return false
	}
	return true
}

// firstInRange returns the lowest-ranked node whose score is inside the range,
// or nil when there is none.
func (sl *skiplist) firstInRange(r rangeSpec) *skiplistNode {
	if !sl.overlaps(r) {
		return nil
	}
	// Move forward while still below the range, at each level.
	x := sl.header
	for i := sl.level - 1; i >= 0; i-- {
		for f := x.level[i].forward; f != nil && !r.gteMin(f.score); f = x.level[i].forward {
			x = f
		}
	}
	// overlaps said something is in range, so this node exists.
	x = x.level[0].forward
	if !r.lteMax(x.score) {
		return nil
	}
	return x
}

// lastInRange returns the highest-ranked node whose score is inside the range,
// or nil when there is none.
func (sl *skiplist) lastInRange(r rangeSpec) *skiplistNode {
	if !sl.overlaps(r) {
		return nil
	}
	// Move forward while still inside the range, at each level.
	x := sl.header
	for i := sl.level - 1; i >= 0; i-- {
		for f := x.level[i].forward; f != nil && r.lteMax(f.score); f = x.level[i].forward {
			x = f
		}
	}
	if x == sl.header || !r.gteMin(x.score) {
		return nil
	}
	return x
}
