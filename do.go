package keel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/core"
)

// Do runs a command the way a client sends it, its name and its arguments,
// for the commands with no method yet, and returns its reply as Go values: a
// string for a simple or bulk string, an int64 for an integer, nil for a
// null, a []any for an array, and an error for an error reply, with Redis's
// text and its category. It runs in the same scope as the methods, and waits
// for durability as they do. A command the engine does not have is Redis's
// "unknown command" error.
//
// Do is experimental: it may change, or go, as the methods arrive.
func (db *DB) Do(ctx context.Context, args ...string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("ERR empty command")
	}
	cmd := &core.Command{Cmd: strings.ToUpper(args[0]), Args: args[1:]}
	if cmd.Cmd != args[0] {
		cmd.Name = args[0]
	}
	var reply replyBuffer
	if err := db.e.Do(ctx, cmd, &reply); err != nil {
		return nil, wrap(err)
	}
	value, err := decodeReply(bufio.NewReader(&reply.Buffer))
	if err != nil {
		return nil, err
	}
	if replyErr, ok := value.(error); ok {
		return nil, replyErr
	}
	return value, nil
}

// replyBuffer is the reply a command writes; nothing is read from it.
type replyBuffer struct{ bytes.Buffer }

// errMalformedReply is a reply this decoder does not read, which a command of
// the engine's never writes.
var errMalformedReply = errors.New("keel: malformed reply")

// decodeReply reads one RESP2 reply. An error reply is returned as a value,
// so that an array of them keeps its places.
func decodeReply(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil || len(line) < 3 || !strings.HasSuffix(line, "\r\n") {
		return nil, errMalformedReply
	}
	kind, body := line[0], line[1:len(line)-2]
	switch kind {
	case '+':
		return body, nil
	case '-':
		return replyError(body), nil
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return nil, errMalformedReply
		}
		return n, nil
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n < -1 {
			return nil, errMalformedReply
		}
		if n == -1 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil || buf[n] != '\r' || buf[n+1] != '\n' {
			return nil, errMalformedReply
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil || n < -1 {
			return nil, errMalformedReply
		}
		if n == -1 {
			return nil, nil
		}
		values := make([]any, n)
		for i := range values {
			if values[i], err = decodeReply(r); err != nil {
				return nil, err
			}
		}
		return values, nil
	}
	return nil, fmt.Errorf("%w: type %q", errMalformedReply, kind)
}

// replyError is an error reply's text as an error of its category.
func replyError(text string) error {
	err := errors.New(text)
	if category := categories[core.CategoryOfReply(text)]; category != nil {
		return &categorized{err: err, category: category}
	}
	switch core.CategoryOfReply(text) {
	case core.CategoryPersistence:
		return fmt.Errorf("%w%s", ErrPersistence, strings.TrimPrefix(text, ErrPersistence.Error()))
	}
	return err
}
