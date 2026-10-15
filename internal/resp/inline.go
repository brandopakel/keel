package resp

import "bytes"

// The inline protocol is Redis's other way to send a command: a line of
// words, split as a shell splits them, ended by a newline. It is what a
// person types at telnet, and what redis-cli --pipe sends between the data it
// was given and the ECHO it ends on. A request is inline when its first byte
// is anything but '*' (networking.c, processInputBuffer).

// InlineMaxSize is the longest inline line, without its newline, that is
// read: Redis's PROTO_INLINE_MAX_SIZE (server.h).
const InlineMaxSize = 64 * 1024

// protocolError is a protocol error in Redis's words. It is ErrProtocol to
// errors.Is, so a caller refuses it and closes the connection as it does any
// other, and its text is the reply.
type protocolError string

func (e protocolError) Error() string        { return string(e) }
func (e protocolError) Is(target error) bool { return target == ErrProtocol }

var (
	// ErrUnbalancedQuotes refuses an inline line whose quotes do not close,
	// or close against the next character (networking.c,
	// CLIENT_READ_UNBALANCED_QUOTES).
	ErrUnbalancedQuotes error = protocolError("ERR Protocol error: unbalanced quotes in request")
	// ErrTooBigInline refuses an inline line longer than InlineMaxSize
	// (networking.c, CLIENT_READ_TOO_BIG_INLINE_REQUEST).
	ErrTooBigInline error = protocolError("ERR Protocol error: too big inline request")
)

// ParseInlineReserved reads the inline request at the front of data, as
// Redis's processInlineBuffer reads one, and reports how many bytes it
// consumed. The line ends at the first "\n", and a "\r" before it is dropped
// with it. Its words are split as sdssplitargs splits them (splitInline).
//
// A line with no words, an empty one among them, comes back as a nil command
// with the bytes it took: Redis skips it. With no newline yet, the line is
// incomplete, or too big once more than InlineMaxSize bytes wait without one;
// a NUL byte before the newline hides it, as it hides it from Redis.
// A line longer than that is refused even when its newline has arrived, so
// the limit holds however the bytes were read; Redis checks it only while the
// newline is missing, after each 16 KiB read.
//
// reserve is charged for what the command will hold before anything of it is
// allocated, as ParseCmdReserved charges it; nil admits everything.
func ParseInlineReserved(data []byte, reserve func(int) bool) (*Command, int, error) {
	newline := bytes.IndexByte(data, '\n')
	end := newline
	if end < 0 {
		end = len(data)
	}
	if bytes.IndexByte(data[:end], 0) >= 0 {
		// Redis finds the newline with strchr, which stops at a NUL byte, so
		// a line holding one has no newline for it: the line waits until
		// the limit refuses it. Nothing of it is run, as nothing is in Redis.
		newline = -1
	}
	if newline < 0 {
		if len(data) > InlineMaxSize {
			return nil, 0, ErrTooBigInline
		}
		return nil, 0, ErrIncompleteFrame
	}
	if newline > InlineMaxSize {
		return nil, 0, ErrTooBigInline
	}
	line := data[:newline]
	if newline > 0 && line[newline-1] == '\r' {
		line = line[:newline-1]
	}
	consumed := newline + 1

	// The first pass only counts, so nothing is allocated for a line that is
	// refused, by its quotes or by the budget.
	argc, size, ok := splitInline(line, nil)
	if !ok {
		return nil, 0, ErrUnbalancedQuotes
	}
	if argc == 0 {
		return nil, consumed, nil
	}
	if reserve != nil {
		charge, ok := inlineCharge(argc, size, line)
		if !ok || !reserve(charge) {
			return nil, 0, ErrRequestAllocation
		}
	}
	words := make([][]byte, 0, argc)
	splitInline(line, func(word []byte) { words = append(words, word) })
	command := allocateDecodedCommand(argc - 1)
	name, sent := ownedUpperCommand(words[0])
	command.Cmd = name
	if name != sent {
		command.Name = sent
	}
	for i, word := range words[1:] {
		command.Args[i] = string(word)
	}
	return command, consumed, nil
}

// inlineCharge is what ParseCmdReserved would charge for argc words of size
// bytes in all: the command, its argument headers, each string, and the
// upper-cased name, which is at most three bytes for each of the line's.
func inlineCharge(argc, size int, line []byte) (int, bool) {
	charge := 80
	for _, n := range []int{(argc - 1) * 16, size, 3 * len(line)} {
		padded, ok := RequestAllocationSize(n)
		if !ok || padded > int(^uint(0)>>1)-charge {
			return 0, false
		}
		charge += padded
	}
	return charge, true
}

// splitInline splits line into words as Redis's sdssplitargs does (sds.c),
// and reports how many there are, their bytes in all, and whether the
// quotes balance. emit, when not nil, is handed each word as it is finished;
// the slice is the word's own.
//
// The rules are sdssplitargs's, quirks and all:
//   - Words are separated by blanks, which are what C's isspace calls space,
//     but a word without quotes ends only at a space, "\t", "\r" or "\n": a
//     "\v" or "\f" inside one is kept.
//   - In double quotes, "\x" and two hex digits is that byte, and a backslash
//     before n, r, t, b or a is that control character; before anything
//     else, it is the character itself.
//   - In single quotes, only "\'" is an escape.
//   - A quote may open in the middle of a word, and what it holds joins the
//     word, but a closing quote must be followed by a blank or the end.
//   - The line is read as a C string, so a NUL byte ends it.
func splitInline(line []byte, emit func([]byte)) (argc, size int, ok bool) {
	// at reads line as a C string reads it, with a NUL past its end.
	at := func(i int) byte {
		if i < len(line) {
			return line[i]
		}
		return 0
	}
	var word []byte
	p := 0
	for {
		for at(p) != 0 && isSpace(at(p)) {
			p++
		}
		if at(p) == 0 {
			return argc, size, true
		}
		inDouble, inSingle, done := false, false, false
		n := 0
		add := func(c byte) {
			n++
			if emit != nil {
				word = append(word, c)
			}
		}
		for !done {
			c := at(p)
			switch {
			case inDouble:
				switch {
				case c == '\\' && at(p+1) == 'x' && isHexDigit(at(p+2)) && isHexDigit(at(p+3)):
					add(hexValue(at(p+2))*16 + hexValue(at(p+3)))
					p += 3
				case c == '\\' && at(p+1) != 0:
					p++
					switch at(p) {
					case 'n':
						add('\n')
					case 'r':
						add('\r')
					case 't':
						add('\t')
					case 'b':
						add('\b')
					case 'a':
						add('\a')
					default:
						add(at(p))
					}
				case c == '"':
					if at(p+1) != 0 && !isSpace(at(p+1)) {
						return 0, 0, false
					}
					done = true
				case c == 0:
					return 0, 0, false
				default:
					add(c)
				}
			case inSingle:
				switch {
				case c == '\\' && at(p+1) == '\'':
					p++
					add('\'')
				case c == '\'':
					if at(p+1) != 0 && !isSpace(at(p+1)) {
						return 0, 0, false
					}
					done = true
				case c == 0:
					return 0, 0, false
				default:
					add(c)
				}
			default:
				switch c {
				case ' ', '\n', '\r', '\t', 0:
					done = true
				case '"':
					inDouble = true
				case '\'':
					inSingle = true
				default:
					add(c)
				}
			}
			if at(p) != 0 {
				p++
			}
		}
		argc++
		size += n
		if emit != nil {
			emit(word)
			word = nil
		}
	}
}

// isSpace is C's isspace in the C locale, which sdssplitargs calls: a byte
// past ASCII is not a space.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' || c == '\f' || c == '\r'
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func hexValue(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}
