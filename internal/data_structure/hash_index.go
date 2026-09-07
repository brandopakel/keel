package data_structure

import (
	"hash/maphash"
	"math/bits"
)

// Large hashes use bounded Go-map leaves. A leaf never grows past this many
// fields, so shrinking it cannot scan capacity from an arbitrarily large hash.
// Branches test distinct hash bits on every path (at most 64). Exact 64-bit
// collisions use bounded leaf chains; they never enlarge one map without bound.
const hashLeafFields = 192

type hashNode struct {
	fields            map[string]string
	left, right, next *hashNode
	bit               uint64
	peak              int
}

type hashIndex struct {
	root         *hashNode
	seed         maphash.Seed
	count        int
	storageBytes uint64
}

// Calibrated Go-map capacity classes for at most 192 string/string entries,
// including the leaf node, map header, table/directory and allocator rounding.
// Payload strings are charged separately. Memory is an estimate, not RSS.
func hashLeafBytes(peak int) uint64 {
	switch {
	case peak == 0:
		return 96
	case peak <= 8:
		return 384
	case peak <= 14:
		return 712
	case peak <= 28:
		return 1288
	case peak <= 56:
		return 2440
	case peak <= 112:
		return 5000
	default:
		return 9608
	}
}

func (h *hashIndex) updatePeak(node *hashNode) {
	if len(node.fields) > node.peak {
		peak := len(node.fields)
		h.storageBytes += hashLeafBytes(peak) - hashLeafBytes(node.peak)
		node.peak = peak
	}
}

func (h *hashIndex) leaf(hash uint64) **hashNode {
	link := &h.root
	for *link != nil && (*link).fields == nil {
		if hash&(*link).bit == 0 {
			link = &(*link).left
		} else {
			link = &(*link).right
		}
	}
	return link
}

func (h *hashIndex) get(field string) (string, bool) {
	return h.getByHash(field, maphash.String(h.seed, field))
}

func (h *hashIndex) getByHash(field string, hash uint64) (string, bool) {
	for node := *h.leaf(hash); node != nil; node = node.next {
		if value, ok := node.fields[field]; ok {
			return value, true
		}
	}
	return "", false
}

func (h *hashIndex) set(field, value string) (string, bool) {
	hash := maphash.String(h.seed, field)
	return h.setByHash(field, value, hash, func(key string) uint64 { return maphash.String(h.seed, key) })
}

// Supplying precomputed hashes also permits exact-collision tests without
// weakening the production seed or adding per-field hash metadata.
func (h *hashIndex) setByHash(field, value string, hash uint64, hashField func(string) uint64) (string, bool) {
	link := h.leaf(hash)
	node := *link
	for current := node; current != nil; current = current.next {
		if old, exists := current.fields[field]; exists {
			current.fields[field] = value
			return old, true
		}
	}
	h.count++
	if node == nil {
		*link = &hashNode{fields: map[string]string{field: value}, peak: 1}
		h.storageBytes += hashLeafBytes(1)
		return "", false
	}
	if node.next != nil {
		if hash != node.bit {
			// A different hash separates from an exact-collision chain without
			// copying or traversing that chain to redistribute its entries.
			h.branch(link, node, &hashNode{fields: map[string]string{field: value}, peak: 1},
				uint64(1)<<(63-bits.LeadingZeros64(hash^node.bit)), hash)
			return "", false
		}
		for node.next != nil && len(node.fields) == hashLeafFields {
			node = node.next
		}
		if len(node.fields) == hashLeafFields {
			node.next = &hashNode{fields: make(map[string]string), bit: hash}
			node = node.next
			h.storageBytes += hashLeafBytes(0)
		}
		node.fields[field] = value
		h.updatePeak(node)
		return "", false
	}
	if len(node.fields) < hashLeafFields {
		node.fields[field] = value
		h.updatePeak(node)
		return "", false
	}
	var different uint64
	for key := range node.fields {
		different |= hashField(key) ^ hash
	}
	if different == 0 {
		node.bit = hash
		node.next = &hashNode{fields: map[string]string{field: value}, bit: hash, peak: 1}
		h.storageBytes += hashLeafBytes(1)
		return "", false
	}
	bit := uint64(1) << (63 - bits.LeadingZeros64(different))
	left := &hashNode{fields: make(map[string]string)}
	right := &hashNode{fields: make(map[string]string)}
	for key, val := range node.fields {
		if hashField(key)&bit == 0 {
			left.fields[key] = val
		} else {
			right.fields[key] = val
		}
	}
	if hash&bit == 0 {
		left.fields[field] = value
	} else {
		right.fields[field] = value
	}
	left.peak, right.peak = len(left.fields), len(right.fields)
	h.storageBytes = h.storageBytes - hashLeafBytes(node.peak) + 48 + hashLeafBytes(left.peak) + hashLeafBytes(right.peak)
	*link = &hashNode{left: left, right: right, bit: bit}
	return "", false
}

func (h *hashIndex) branch(link **hashNode, old, added *hashNode, bit, hash uint64) {
	h.storageBytes += 48 + hashLeafBytes(added.peak)
	if hash&bit == 0 {
		*link = &hashNode{left: added, right: old, bit: bit}
	} else {
		*link = &hashNode{left: old, right: added, bit: bit}
	}
}

func (h *hashIndex) del(field string) (string, bool) {
	return h.delByHash(field, maphash.String(h.seed, field))
}

func (h *hashIndex) delByHash(field string, hash uint64) (string, bool) {
	var path [64]**hashNode
	depth := 0
	link := &h.root
	for *link != nil && (*link).fields == nil {
		path[depth] = link
		depth++
		if hash&(*link).bit == 0 {
			link = &(*link).left
		} else {
			link = &(*link).right
		}
	}
	var removed string
	found := false
	for *link != nil {
		node := *link
		value, exists := node.fields[field]
		if !exists {
			link = &node.next
			continue
		}
		delete(node.fields, field)
		removed, found = value, true
		h.count--
		if len(node.fields) == 0 {
			h.storageBytes -= hashLeafBytes(node.peak)
			*link = node.next
		} else if node.peak >= 16 && len(node.fields) <= node.peak/4 {
			next := make(map[string]string, len(node.fields))
			for key, value := range node.fields {
				next[key] = value
			}
			h.storageBytes -= hashLeafBytes(node.peak) - hashLeafBytes(len(next))
			node.fields, node.peak = next, len(next)
		}
		break
	}
	if !found {
		return "", false
	}
	// Collapse empty branches and merge at most one pair of small leaves.
	// One deletion never recursively copies all surviving collection fields.
	merged := false
	for i := depth - 1; i >= 0; i-- {
		link := path[i]
		node := *link
		if node.left == nil {
			h.storageBytes -= 48
			*link = node.right
		} else if node.right == nil {
			h.storageBytes -= 48
			*link = node.left
		} else if !merged && node.left.fields != nil && node.right.fields != nil &&
			node.left.next == nil && node.right.next == nil &&
			len(node.left.fields)+len(node.right.fields) <= hashLeafFields/2 {
			fields := make(map[string]string, len(node.left.fields)+len(node.right.fields))
			for key, value := range node.left.fields {
				fields[key] = value
			}
			for key, value := range node.right.fields {
				fields[key] = value
			}
			h.storageBytes = h.storageBytes - 48 - hashLeafBytes(node.left.peak) - hashLeafBytes(node.right.peak) + hashLeafBytes(len(fields))
			*link = &hashNode{fields: fields, peak: len(fields)}
			merged = true
		}
	}
	return removed, true
}

func (h *hashIndex) visit(yield func(string, string) bool) {
	var pending [65]*hashNode
	pending[0] = h.root
	depth := 1
	for depth > 0 {
		depth--
		node := pending[depth]
		pending[depth] = nil
		if node == nil {
			continue
		}
		if node.fields == nil {
			pending[depth], pending[depth+1] = node.left, node.right
			depth += 2
			continue
		}
		for key, value := range node.fields {
			if !yield(key, value) {
				return
			}
		}
		if node.next != nil {
			pending[depth] = node.next
			depth++
		}
	}
}
