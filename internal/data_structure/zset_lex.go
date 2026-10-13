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

// This file is a Go translation of the lexicographic ranges of Redis 8.10.1's
// src/t_zset.c (zslParseLexRangeItem, sdscmplex, zslLexValueGteMin,
// zslLexValueLteMax, zslIsInLexRange, zslNthInLexRange), which carries the
// licence above. The translation was made for keel by Brando Pakel in 2026
// and is distributed under the same terms.

package data_structure

import "strings"

// A lexicographic range compares members alone, byte by byte, as if every
// score were equal: it is meant for a sorted set whose members all share a
// score, and over any other it answers what Redis answers, walking the list in
// its order and comparing only members.

// LexBound is one end of a lexicographic range: a member, included or not,
// or the end of everything below (-) or above (+).
type LexBound struct {
	Member    string
	Exclusive bool
	// NegInf and PosInf are "-" and "+": below and above every member.
	NegInf, PosInf bool
}

// LexRange is a lexicographic range, Min to Max.
type LexRange struct{ Min, Max LexBound }

// ParseLexBound reads one end as ZRANGEBYLEX takes it: "+", "-", or a member
// after "(" (exclusive) or "[" (inclusive). Anything else is not a range.
func ParseLexBound(text string) (LexBound, bool) {
	switch {
	case text == "+":
		return LexBound{PosInf: true}, true
	case text == "-":
		return LexBound{NegInf: true}, true
	case strings.HasPrefix(text, "("):
		return LexBound{Member: text[1:], Exclusive: true}, true
	case strings.HasPrefix(text, "["):
		return LexBound{Member: text[1:]}, true
	}
	return LexBound{}, false
}

// compare orders a member against a bound, or two bounds' members, as
// sdscmplex does: "-" below everything and "+" above.
func lexCompare(a, b LexBound) int {
	switch {
	case a.NegInf == b.NegInf && a.PosInf == b.PosInf && (a.NegInf || a.PosInf):
		return 0
	case a.NegInf || b.PosInf:
		return -1
	case a.PosInf || b.NegInf:
		return 1
	}
	return strings.Compare(a.Member, b.Member)
}

func (r LexRange) gteMin(member string) bool {
	c := lexCompare(LexBound{Member: member}, r.Min)
	if r.Min.Exclusive {
		return c > 0
	}
	return c >= 0
}

func (r LexRange) lteMax(member string) bool {
	c := lexCompare(LexBound{Member: member}, r.Max)
	if r.Max.Exclusive {
		return c < 0
	}
	return c <= 0
}

// empty is a range that can hold nothing: min above max, or equal with
// either end excluded.
func (r LexRange) empty() bool {
	c := lexCompare(r.Min, r.Max)
	return c > 0 || (c == 0 && (r.Min.Exclusive || r.Max.Exclusive))
}

// lexOverlaps is zslIsInLexRange: whether the list may hold anything in r.
func (sl *skiplist) lexOverlaps(r LexRange) bool {
	if r.empty() {
		return false
	}
	if sl.tail == nil || !r.gteMin(sl.tail.ele) {
		return false
	}
	if head := sl.first(); head == nil || !r.lteMax(head.ele) {
		return false
	}
	return true
}

// firstInLexRange returns the lowest-ranked node inside r, or nil.
func (sl *skiplist) firstInLexRange(r LexRange) *skiplistNode {
	if !sl.lexOverlaps(r) {
		return nil
	}
	x := sl.header
	for i := sl.level - 1; i >= 0; i-- {
		for f := x.level[i].forward; f != nil && !r.gteMin(f.ele); f = x.level[i].forward {
			x = f
		}
	}
	x = x.level[0].forward
	if x == nil || !r.lteMax(x.ele) {
		return nil
	}
	return x
}

// lastInLexRange returns the highest-ranked node inside r, or nil.
func (sl *skiplist) lastInLexRange(r LexRange) *skiplistNode {
	if !sl.lexOverlaps(r) {
		return nil
	}
	x := sl.header
	for i := sl.level - 1; i >= 0; i-- {
		for f := x.level[i].forward; f != nil && r.lteMax(f.ele); f = x.level[i].forward {
			x = f
		}
	}
	if x == sl.header || !r.gteMin(x.ele) {
		return nil
	}
	return x
}

// CountByLex counts the members inside r, by the ranks of its ends.
func (zs *ZSet) CountByLex(r LexRange) int64 {
	first, last := zs.sl.firstInLexRange(r), zs.sl.lastInLexRange(r)
	if first == nil || last == nil {
		return 0
	}
	lo, hi := zs.sl.rank(first.score, first.ele), zs.sl.rank(last.score, last.ele)
	if hi < lo {
		return 0
	}
	return int64(hi - lo + 1)
}

// VisitRangeByLex visits the members inside r, skipping offset of them and
// stopping after count (all when count is negative), from the lowest or,
// reversed, from the highest. It walks node by node from the first member in
// range, as Redis does, and stops at the first member past the range: over a
// set whose scores differ that can be before members a byte comparison alone
// would include. VisitRangeByRank's early-stop and no-mutation contract holds.
func (zs *ZSet) VisitRangeByLex(r LexRange, offset, count int, reverse bool, yield func(string, float64) bool) {
	if offset < 0 || count == 0 {
		return
	}
	var x *skiplistNode
	if reverse {
		x = zs.sl.lastInLexRange(r)
	} else {
		x = zs.sl.firstInLexRange(r)
	}
	for x != nil && offset > 0 {
		offset--
		if reverse {
			x = x.backward
		} else {
			x = x.level[0].forward
		}
	}
	for x != nil && count != 0 {
		if reverse {
			if !r.gteMin(x.ele) {
				return
			}
		} else if !r.lteMax(x.ele) {
			return
		}
		if !yield(x.ele, x.score) {
			return
		}
		count--
		if reverse {
			x = x.backward
		} else {
			x = x.level[0].forward
		}
	}
}

// MemberAtRank is the member at a 0-based rank from the lowest score, and its
// score; false for a rank outside the set.
func (zs *ZSet) MemberAtRank(rank int) (string, float64, bool) {
	if rank < 0 || rank >= zs.Len() {
		return "", 0, false
	}
	x := zs.sl.nodeAtRank(uint64(rank) + 1)
	if x == nil {
		return "", 0, false
	}
	return x.ele, x.score, true
}
