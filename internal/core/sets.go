package core

import "github.com/brandopakel/keel/internal/data_structure"

// Typed operations: sets (phase 4 of docs/embedding-plan.md; the family
// recipe is docs/phase4-family-recipe.md).
//
// Redis has no empty set: removing the last member removes the key
// (setSettle), and a set is created only once a member will be put in it.
// Each write stages the record develop logged for it, byte for byte: SADD
// and SREM with their members as given, and SPOP the SREM of what it popped.
// SADD that adds nothing and SREM that removes nothing are logged although
// they change nothing, which Redis does not (t_set.c counts a change only
// when there was one); the family's missing-commands step drops them.

// setFor is the set key holds, and false for none.
func (e *Engine) setFor(key string) (*data_structure.Set, bool) {
	return e.setStore.Get(key)
}

// setSettle records that a set was changed in place: an emptied set is
// removed, and any other re-measured for the memory budget.
func (e *Engine) setSettle(key string, s *data_structure.Set) {
	if s.Len() == 0 {
		e.setStore.Delete(key)
		return
	}
	e.setStore.Resize(key)
}

// opSAdd adds each of members to the set key holds, creating it, and answers
// how many were not there before. It is recorded as the SADD it is.
func (e *Engine) opSAdd(key string, members []string) int64 {
	s, ok := e.setFor(key)
	if !ok {
		s = data_structure.NewSet()
		e.setStore.Put(key, s)
	}
	added := s.Add(members...)
	// Members change the set's size without going through Put, so the keyspace
	// has to re-measure it or the memory budget keeps believing the old figure.
	e.setStore.Resize(key)
	e.aofRecordKeyArgs("SADD", key, members)
	return int64(added)
}

// opSRem removes each of members from the set key holds, removing the key
// with its last member, and answers how many it removed. It is recorded as
// SREM whatever it removed, as develop logs it (see above).
func (e *Engine) opSRem(key string, members []string) int64 {
	e.aofRecordKeyArgs("SREM", key, members)
	s, ok := e.setFor(key)
	if !ok {
		// Nothing to remove from, and nothing is created to say so.
		return 0
	}
	removed := s.Remove(members...)
	e.setSettle(key, s)
	return int64(removed)
}

// opSCard counts the members of the set key holds.
func (e *Engine) opSCard(key string) int64 {
	s, ok := e.setFor(key)
	if !ok {
		return 0
	}
	return int64(s.Len())
}

// opSIsMember reports whether the set key holds has member.
func (e *Engine) opSIsMember(key, member string) bool {
	s, ok := e.setFor(key)
	return ok && s.Contains(member)
}
