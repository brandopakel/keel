package keel

import (
	"errors"

	"github.com/brandopakel/keel/internal/core"
)

// The categories of the errors this package returns. An error keeps the text
// Redis would answer with, and errors.Is matches it against its category.
var (
	// ErrNotFound is a key that holds nothing, where Redis answers nil.
	ErrNotFound = errors.New("keel: key not found")
	// ErrWrongType is a key that holds another type.
	ErrWrongType = errors.New("keel: key holds the wrong kind of value")
	// ErrNotInteger is a value that is not an integer, or not in range.
	ErrNotInteger = errors.New("keel: value is not an integer or out of range")
	// ErrOverflow is an increment past the integer's range, or one that would
	// make a float infinite.
	ErrOverflow = errors.New("keel: increment would overflow")
	// ErrOutOfMemory is a write the memory bound refuses, or the bound on
	// what one key may allocate.
	ErrOutOfMemory = errors.New("keel: out of memory")
	// ErrReadOnly is a write a replica refuses.
	ErrReadOnly = errors.New("keel: instance is read-only")
	// ErrPersistence is a log that has failed, with Redis's MISCONF text and
	// the failure. Under FsyncEverySec and FsyncNo, writes are refused while
	// the log is retried and reads are served; under FsyncAlways every call
	// is refused until the DB is reopened.
	ErrPersistence = core.ErrPersistence
	// ErrClosed is a DB that has been closed.
	ErrClosed = core.ErrClosed
	// ErrLocked is a log another instance has open.
	ErrLocked = core.ErrLocked
)

// categorized is an error of the engine's, with its own text, that errors.Is
// matches against the category it belongs to.
type categorized struct {
	err      error
	category error
}

func (c *categorized) Error() string        { return c.err.Error() }
func (c *categorized) Is(target error) bool { return target == c.category }
func (c *categorized) Unwrap() error        { return c.err }

// categories maps the engine's categories onto this package's sentinels.
// ErrPersistence, ErrClosed and ErrLocked are the engine's own, which its
// errors already match.
var categories = map[core.ErrorCategory]error{
	core.CategoryWrongType:   ErrWrongType,
	core.CategoryNotInteger:  ErrNotInteger,
	core.CategoryOverflow:    ErrOverflow,
	core.CategoryOutOfMemory: ErrOutOfMemory,
	core.CategoryReadOnly:    ErrReadOnly,
}

// wrap is err as this package returns it: matched to its category where it
// has one, its text unchanged.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	if category := categories[core.CategoryOf(err)]; category != nil {
		return &categorized{err: err, category: category}
	}
	return err
}
