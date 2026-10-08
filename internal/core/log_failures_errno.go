//go:build !plan9

package core

import (
	"errors"
	"io"
	"syscall"
)

// errnoText is the errno err carries, or ENOSPC for a short write, as Go
// words it.
func errnoText(err error) (string, bool) {
	var errno syscall.Errno
	switch {
	case errors.As(err, &errno):
	case errors.Is(err, io.ErrShortWrite):
		errno = syscall.ENOSPC
	default:
		return "", false
	}
	return errno.Error(), true
}
