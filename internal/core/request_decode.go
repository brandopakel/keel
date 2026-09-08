package core

import (
	"errors"
	"strings"
)

var ErrRequestAllocation = errors.New("ERR request allocation budget exhausted")

// RequestAllocationSize includes conservative size-class/rounding headroom.
// It is an admission charge for owned allocations, not measured heap or RSS.
func RequestAllocationSize(n int) (int, bool) {
	const maxInt = int(^uint(0) >> 1)
	if n < 0 || n > maxInt-n/4-64 {
		return 0, false
	}
	if n == 0 {
		return 0, true
	}
	return n + n/4 + 64, true
}

type commandSpan struct{ start, end int }

// ParseCmdReserved validates one complete frame before reserving and copying
// its strings. Incomplete large frames therefore do not repeatedly copy keys or
// earlier arguments. The common sixteen-field layout stays on the stack; larger
// complete frames are walked again after admission rather than growing metadata
// from an untrusted count. The callback must not mutate data or retain it.
func ParseCmdReserved(data []byte, reserve func(int) bool) (*Command, int, error) {
	if len(data) == 0 || data[0] != '*' {
		return nil, 0, commandShapeError(data)
	}
	r := frameReader{data: data, pos: 1}
	n, err := r.integer()
	if err != nil {
		return nil, 0, err
	}
	if n <= 0 || n > maxMultiBulkLength {
		return nil, 0, ErrProtocol
	}
	first := r.pos
	var spans [16]commandSpan
	charge := 64 // Command and the caller's pointer to it.
	add := func(size int) bool {
		padded, ok := RequestAllocationSize(size)
		if !ok || padded > int(^uint(0)>>1)-charge {
			return false
		}
		charge += padded
		return true
	}
	if !add(int(n-1) * 16) {
		return nil, 0, ErrRequestAllocation
	}
	for i := 0; i < int(n); i++ {
		span, err := r.commandStringSpan()
		if err != nil {
			// The reference decoder validates an entire unusual RESP value
			// before rejecting its command shape. Preserve that distinction
			// between incomplete and invalid without constructing any values.
			return nil, 0, commandShapeError(data)
		}
		if i < len(spans) {
			spans[i] = span
		}
		size := span.end - span.start
		if !add(size) {
			return nil, 0, ErrRequestAllocation
		}
		// Unicode uppercasing can grow and replace its backing allocation.
		// Reserve that temporary copy as well as the owned command string.
		if i == 0 && (size > int(^uint(0)>>1)/3 || !add(3*size)) {
			return nil, 0, ErrRequestAllocation
		}
	}
	consumed := r.pos
	if reserve != nil && !reserve(charge) {
		return nil, 0, ErrRequestAllocation
	}
	args := make([]string, int(n)-1)
	var name string
	r.pos = first
	for i := 0; i < int(n); i++ {
		var span commandSpan
		if n <= int64(len(spans)) {
			span = spans[i]
		} else {
			span, err = r.commandStringSpan()
			if err != nil {
				return nil, 0, err
			}
		}
		value := string(data[span.start:span.end])
		if i == 0 {
			name = strings.ToUpper(value)
		} else {
			args[i-1] = value
		}
	}
	return &Command{Cmd: name, Args: args}, consumed, nil
}

func (r *frameReader) commandStringSpan() (commandSpan, error) {
	if r.pos >= len(r.data) {
		return commandSpan{}, ErrIncompleteFrame
	}
	kind := r.data[r.pos]
	r.pos++
	switch kind {
	case '+', '-':
		start := r.pos
		line, err := r.line()
		return commandSpan{start, start + len(line)}, err
	case '$':
		n, err := r.integer()
		if err != nil {
			return commandSpan{}, err
		}
		if n < -1 || n > maxBulkLength {
			return commandSpan{}, ErrProtocol
		}
		if n == -1 {
			return commandSpan{r.pos, r.pos}, nil
		}
		if n > int64(len(r.data)-r.pos)-2 {
			return commandSpan{}, ErrIncompleteFrame
		}
		span := commandSpan{r.pos, r.pos + int(n)}
		if r.data[span.end] != '\r' || r.data[span.end+1] != '\n' {
			return commandSpan{}, ErrProtocol
		}
		r.pos = span.end + 2
		return span, nil
	default:
		return commandSpan{}, ErrProtocol
	}
}

func commandShapeError(data []byte) error {
	r := frameReader{data: data}
	if err := r.skipValue(); err != nil {
		return err
	}
	return ErrProtocol
}

// skipValue mirrors value's framing and depth rules without allocating decoded
// strings, interfaces or arrays. It is used only for invalid command shapes.
func (r *frameReader) skipValue() error {
	if r.pos >= len(r.data) {
		return ErrIncompleteFrame
	}
	switch r.data[r.pos] {
	case '+', '-', '$':
		_, err := r.commandStringSpan()
		return err
	case ':':
		r.pos++
		_, err := r.integer()
		return err
	case '*':
		r.pos++
		n, err := r.integer()
		if err != nil {
			return err
		}
		if n < -1 || n > maxMultiBulkLength {
			return ErrProtocol
		}
		if n == -1 {
			return nil
		}
		if r.depth >= maxArrayDepth {
			return ErrProtocol
		}
		r.depth++
		defer func() { r.depth-- }()
		for i := int64(0); i < n; i++ {
			if err := r.skipValue(); err != nil {
				return err
			}
		}
		return nil
	default:
		return ErrProtocol
	}
}
