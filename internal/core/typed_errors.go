package core

import (
	"errors"
	"strings"
)

// ErrorCategory is the public package's name for a kind of error a typed
// call returns. An error keeps its own text - Redis's wire error for the
// typed operations' sentinels - and the public package lets errors.Is match
// it against the category's sentinel (keel.ErrWrongType and the rest).
type ErrorCategory uint8

// The categories, in the order the plan lists them (docs/embedding-plan.md,
// "API").
const (
	// CategoryNone is an error of no category: a refusal of an argument, in
	// the command's own words.
	CategoryNone ErrorCategory = iota
	// CategoryWrongType is a key that holds another type: WRONGTYPE, and the
	// refusals of commands that say so in words of their own.
	CategoryWrongType
	// CategoryNotInteger is a value that is not an integer, or not in range.
	CategoryNotInteger
	// CategoryOverflow is an increment that would leave the integer's range,
	// or make a float infinite.
	CategoryOverflow
	// CategoryOutOfMemory is a write the memory bound, or the bound on one
	// key's allocation, refuses.
	CategoryOutOfMemory
	// CategoryReadOnly is a write a replica refuses.
	CategoryReadOnly
	// CategoryPersistence is a log that has failed: Redis's MISCONF.
	CategoryPersistence
	// CategoryClosed is an instance that has been closed.
	CategoryClosed
	// CategoryLocked is a log another instance holds.
	CategoryLocked
)

// CategoryOf is the category of err, CategoryNone for none.
func CategoryOf(err error) ErrorCategory {
	switch {
	case err == nil:
		return CategoryNone
	case errors.Is(err, errWrongType), errors.Is(err, errNotHLL), errors.Is(err, errLCSType):
		return CategoryWrongType
	case errors.Is(err, errNotAnInteger):
		return CategoryNotInteger
	case errors.Is(err, errIncrOverflow), errors.Is(err, errDecrOverflow), errors.Is(err, errFloatInfinite):
		return CategoryOverflow
	case errors.Is(err, errTooLargeForBudget), errors.Is(err, errTooLargeForOneKey):
		return CategoryOutOfMemory
	case errors.Is(err, errReadOnlyReplica):
		return CategoryReadOnly
	case errors.Is(err, ErrPersistence):
		return CategoryPersistence
	case errors.Is(err, ErrClosed):
		return CategoryClosed
	case errors.Is(err, ErrLocked):
		return CategoryLocked
	}
	return CategoryNone
}

// CategoryOfReply is the category of an error reply's text, without its
// leading '-': the experimental Do's, whose errors arrive as the wire's
// words. Redis's prefixes name most of them; the rest are known by their
// whole text.
func CategoryOfReply(text string) ErrorCategory {
	switch {
	case strings.HasPrefix(text, "WRONGTYPE "), text == errLCSType.Error():
		return CategoryWrongType
	case strings.HasPrefix(text, "OOM "):
		return CategoryOutOfMemory
	case strings.HasPrefix(text, "READONLY "):
		return CategoryReadOnly
	case strings.HasPrefix(text, "MISCONF "):
		return CategoryPersistence
	case text == errNotAnInteger.Error():
		return CategoryNotInteger
	case text == errIncrOverflow.Error(), text == errDecrOverflow.Error(), text == errFloatInfinite.Error():
		return CategoryOverflow
	case text == errTooLargeForOneKey.Error():
		return CategoryOutOfMemory
	}
	return CategoryNone
}
