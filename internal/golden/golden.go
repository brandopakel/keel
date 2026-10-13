// Package golden is the fixture format of the command corpora: what every
// command answers, in RESP2 and in RESP3, and what it writes to the log,
// captured from one revision and held to by every later one (phase 4 of
// docs/embedding-plan.md). internal/core keeps the corpus of the commands it
// runs and internal/server the corpus of the connection commands it answers
// itself; both write and read this one format, so a difference in either is
// shown the same way.
//
// A fixture is text, one line a thing, so that a change to a reply is a diff
// a reviewer can read:
//
//	case strings/set-nx
//	> SET "k" "v" "NX"
//	2 "+OK\r\n"
//	3 "+OK\r\n"
//	~ "SET" "k" "v"
//	> SMEMBERS "s" @sorted
//	2 "*2\r\n$1\r\na\r\n$1\r\nb\r\n"
//	3 "~2\r\n$1\r\na\r\n$1\r\nb\r\n"
//	! expire-cycle
//	~ "DEL" "brief"
//
// A case runs on an empty engine. Each `>` line is a command as a client
// sends it, with the flags of the normalizers applied to its reply (see
// Normalize); `2` and `3` are the reply on a RESP2 and a RESP3 connection;
// each `~` line is one record the command, or the harness's step before it,
// appended to the log, as the strings it holds; and `!` is a step of the
// harness between commands. A string longer than a line can hold is written
// as its digest (Token). Lines beginning `#` are comments.
package golden

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Step is one line of a case's session: a command with its replies and
// records, or a directive of the harness.
type Step struct {
	// Directive is the harness's step, as "sleep 5ms", when the step is not
	// a command. Such a step has no replies, and records only when it made
	// the engine append some, as an expiry cycle does.
	Directive string
	// Args is the command as sent, its name first.
	Args []string
	// Flags are the normalizers applied to the command's reply before it is
	// compared, in the order applied.
	Flags []string
	// Replies is the reply on a RESP2 and on a RESP3 connection, normalized,
	// as the bytes on the wire.
	Replies [2]string
	// Records is what the command appended to the log: each record as the
	// strings it holds.
	Records [][]string
}

// Case is one session, named family/name.
type Case struct {
	Name  string
	Steps []Step
}

// Token writes s as a fixture holds it: quoted, or, past the length a line
// should hold, as its digest and length. A reply or an argument of a few
// megabytes is compared by its digest, and the fixture stays readable.
func Token(s string) string {
	if len(s) <= TokenLimit {
		return strconv.Quote(s)
	}
	return Digest(s)
}

// TokenLimit is the longest string a fixture holds as it is.
const TokenLimit = 1024

// Digest is the token of a string too long to hold: sha256:<first 16
// hex>/<length>.
func Digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:8]) + "/" + strconv.Itoa(len(s))
}

// Write writes cases as a fixture.
func Write(w io.Writer, cases []Case) error {
	b := bufio.NewWriter(w)
	for i, c := range cases {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(b, "case %s\n", c.Name)
		for _, s := range c.Steps {
			if s.Directive != "" {
				fmt.Fprintf(b, "! %s\n", s.Directive)
				writeRecords(b, s.Records)
				continue
			}
			b.WriteString(">")
			for i, a := range s.Args {
				b.WriteByte(' ')
				if i == 0 {
					b.WriteString(strconv.Quote(a))
				} else {
					b.WriteString(Token(a))
				}
			}
			for _, f := range s.Flags {
				b.WriteString(" @")
				b.WriteString(f)
			}
			b.WriteString("\n")
			fmt.Fprintf(b, "2 %s\n3 %s\n", Token(s.Replies[0]), Token(s.Replies[1]))
			writeRecords(b, s.Records)
		}
	}
	return b.Flush()
}

func writeRecords(b *bufio.Writer, records [][]string) {
	for _, r := range records {
		b.WriteString("~")
		for _, part := range r {
			b.WriteByte(' ')
			b.WriteString(Token(part))
		}
		b.WriteString("\n")
	}
}

// Parse reads a fixture.
func Parse(r io.Reader) ([]Case, error) {
	var cases []Case
	var current *Case
	var step *Step
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kind, rest, _ := strings.Cut(line, " ")
		fail := func(format string, args ...any) ([]Case, error) {
			return nil, fmt.Errorf("line %d: %s", n, fmt.Sprintf(format, args...))
		}
		switch kind {
		case "case":
			if rest == "" {
				return fail("a case needs a name")
			}
			cases = append(cases, Case{Name: rest})
			current = &cases[len(cases)-1]
			step = nil
		case "!":
			if current == nil {
				return fail("a step outside any case")
			}
			current.Steps = append(current.Steps, Step{Directive: rest})
			step = &current.Steps[len(current.Steps)-1]
		case ">":
			if current == nil {
				return fail("a step outside any case")
			}
			tokens, flags, err := tokens(rest)
			if err != nil || len(tokens) == 0 {
				return fail("a command: %v", err)
			}
			current.Steps = append(current.Steps, Step{Args: tokens, Flags: flags})
			step = &current.Steps[len(current.Steps)-1]
		case "2", "3":
			if step == nil || step.Directive != "" {
				return fail("a reply with no command")
			}
			value, err := untoken(rest)
			if err != nil {
				return fail("a reply: %v", err)
			}
			step.Replies[kind[0]-'2'] = value
		case "~":
			if step == nil {
				return fail("a record with no command")
			}
			parts, flags, err := tokens(rest)
			if err != nil || len(flags) > 0 {
				return fail("a record: %v", err)
			}
			step.Records = append(step.Records, parts)
		default:
			return fail("unknown line %q", line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return cases, nil
}

// tokens reads the quoted strings of a line, and the @flags after them.
func tokens(line string) (values, flags []string, err error) {
	values = []string{}
	for line = strings.TrimLeft(line, " "); line != ""; line = strings.TrimLeft(line, " ") {
		if strings.HasPrefix(line, "@") {
			flag, rest, _ := strings.Cut(line[1:], " ")
			flags = append(flags, flag)
			line = rest
			continue
		}
		if len(flags) > 0 {
			return nil, nil, errors.New("a value after the flags")
		}
		if strings.HasPrefix(line, "sha256:") {
			token, rest, _ := strings.Cut(line, " ")
			values = append(values, token)
			line = rest
			continue
		}
		quoted, err := strconv.QuotedPrefix(line)
		if err != nil {
			return nil, nil, fmt.Errorf("%q: %w", line, err)
		}
		value, err := strconv.Unquote(quoted)
		if err != nil {
			return nil, nil, err
		}
		values = append(values, value)
		line = line[len(quoted):]
	}
	return values, flags, nil
}

// untoken reads one token: a quoted string, or a digest kept as it is.
func untoken(token string) (string, error) {
	if strings.HasPrefix(token, "sha256:") {
		return token, nil
	}
	return strconv.Unquote(token)
}

// Held is s as the fixture holds it, for comparing a captured string with a
// parsed one: a short string is itself, a long one its digest.
func Held(s string) string {
	if len(s) <= TokenLimit {
		return s
	}
	return Digest(s)
}

// The normalizers. Each is named by the flag a step carries, and rewrites a
// reply so that two runs of a command whose answer is right either way come
// out the same: a set's members in any order, a hash's fields in any order, a
// TTL counted from a different instant.
const (
	// FlagSorted sorts the elements of every array or set whose elements
	// are all scalars: SMEMBERS, HKEYS, KEYS, SCAN's keys, SPOP's members.
	FlagSorted = "sorted"
	// FlagPairs sorts a map's pairs by key, and a RESP2 flat array of pairs
	// the same way: HGETALL, and the INFO maps.
	FlagPairs = "pairs"
	// FlagValues replaces every value of a map, and of a RESP2 flat array
	// of pairs, with the integer 0: a report whose fields are the contract
	// and whose numbers are estimates, MEMORY STATS.
	FlagValues = "values"
	// FlagMasked keeps only a reply's type byte: a reply whose content is
	// the clock's or the machine's, INFO's, PTTL's.
	FlagMasked = "masked"
	// FlagTTLSeconds and FlagTTLMillis round a non-negative integer reply to
	// the nearest whole hour in its unit, so that a TTL set hours ahead
	// reads the same however many milliseconds have passed since.
	FlagTTLSeconds = "ttl-s"
	FlagTTLMillis  = "ttl-ms"
	// FlagRecordsSorted sorts the arguments after the key of each record the
	// command appended, for a pop that removes members in a random order.
	FlagRecordsSorted = "records-sorted"
	// FlagZero, written zero:<key>, replaces the value that follows the key
	// named, in a map or a RESP2 flat array of pairs, with the integer 0: a
	// field whose number is the machine's, a filter's Size.
	FlagZero = "zero:"
)

// Normalize applies flags to reply, in order, and returns the result. A
// flag Normalize does not know is an error, as is a reply that is not RESP.
func Normalize(reply []byte, flags []string) ([]byte, error) {
	for _, flag := range flags {
		var err error
		switch {
		case flag == FlagRecordsSorted:
			// Applied to the records, not the reply.
		case flag == FlagMasked:
			if len(reply) > 0 {
				reply = reply[:1]
			}
		case flag == FlagTTLSeconds:
			reply, err = roundTTL(reply, 3600)
		case flag == FlagTTLMillis:
			reply, err = roundTTL(reply, 3600_000)
		case flag == FlagSorted || flag == FlagPairs || flag == FlagValues:
			var n *node
			if n, err = parse(reply); err == nil {
				n.rewrite(flag, "")
				reply = n.encode(nil)
			}
		case strings.HasPrefix(flag, FlagZero) && len(flag) > len(FlagZero):
			var n *node
			if n, err = parse(reply); err == nil {
				n.rewrite(FlagZero, strings.TrimPrefix(flag, FlagZero))
				reply = n.encode(nil)
			}
		default:
			return nil, fmt.Errorf("unknown normalizer %q", flag)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", flag, err)
		}
	}
	return reply, nil
}

// NormalizeRecords applies the records' flags.
func NormalizeRecords(records [][]string, flags []string) [][]string {
	if !slices.Contains(flags, FlagRecordsSorted) {
		return records
	}
	out := make([][]string, len(records))
	for i, r := range records {
		r = slices.Clone(r)
		if len(r) > 2 {
			sort.Strings(r[2:])
		}
		out[i] = r
	}
	return out
}

// roundTTL rounds an integer reply of unit per hour to the nearest hour. A
// negative reply, TTL's -1 and -2, is left alone.
func roundTTL(reply []byte, unit int64) ([]byte, error) {
	if len(reply) < 3 || reply[0] != ':' {
		return nil, fmt.Errorf("not an integer reply: %q", reply)
	}
	n, err := strconv.ParseInt(string(bytes.TrimSuffix(reply[1:], []byte("\r\n"))), 10, 64)
	if err != nil {
		return nil, err
	}
	if n >= 0 {
		n = (n + unit/2) / unit * unit
	}
	return []byte(":" + strconv.FormatInt(n, 10) + "\r\n"), nil
}

// node is one RESP value, RESP2 or RESP3: a scalar as its bytes on the wire,
// or an aggregate with its items.
type node struct {
	kind byte
	// raw is a scalar's whole encoding, header and payload.
	raw []byte
	// items are an aggregate's values; a map's are its keys and values
	// alternating. attributes (|) are not sent by anything here.
	items []*node
	null  bool
}

func (n *node) scalar() bool { return n.items == nil && !n.aggregate() }

func (n *node) aggregate() bool {
	return (n.kind == '*' || n.kind == '~' || n.kind == '%' || n.kind == '>') && !n.null
}

// parse reads one whole reply.
func parse(reply []byte) (*node, error) {
	p := replyReader{data: reply}
	n, err := p.value()
	if err != nil {
		return nil, err
	}
	if p.pos != len(reply) {
		return nil, fmt.Errorf("%d bytes after the reply", len(reply)-p.pos)
	}
	return n, nil
}

type replyReader struct {
	data []byte
	pos  int
}

func (p *replyReader) line() ([]byte, error) {
	rest := p.data[p.pos:]
	end := bytes.Index(rest, []byte("\r\n"))
	if end < 0 {
		return nil, errors.New("no CRLF")
	}
	p.pos += end + 2
	return rest[:end], nil
}

func (p *replyReader) value() (*node, error) {
	if p.pos >= len(p.data) {
		return nil, errors.New("empty reply")
	}
	start := p.pos
	kind := p.data[p.pos]
	p.pos++
	line, err := p.line()
	if err != nil {
		return nil, err
	}
	n := &node{kind: kind}
	switch kind {
	case '+', '-', ':', '_', ',', '#', '(':
		n.raw = p.data[start:p.pos]
		return n, nil
	case '$', '=', '!':
		size, err := strconv.Atoi(string(line))
		if err != nil {
			return nil, err
		}
		if size < 0 {
			n.raw, n.null = p.data[start:p.pos], true
			return n, nil
		}
		if p.pos+size+2 > len(p.data) {
			return nil, errors.New("short bulk string")
		}
		p.pos += size + 2
		n.raw = p.data[start:p.pos]
		return n, nil
	case '*', '~', '%', '>':
		count, err := strconv.Atoi(string(line))
		if err != nil {
			return nil, err
		}
		if count < 0 {
			n.raw, n.null = p.data[start:p.pos], true
			return n, nil
		}
		if kind == '%' {
			count *= 2
		}
		n.items = make([]*node, 0, count)
		for i := 0; i < count; i++ {
			item, err := p.value()
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, item)
		}
		return n, nil
	}
	return nil, fmt.Errorf("unknown reply type %q", kind)
}

// encode writes n back as it was read, items in their current order.
func (n *node) encode(dst []byte) []byte {
	if !n.aggregate() {
		return append(dst, n.raw...)
	}
	count := len(n.items)
	if n.kind == '%' {
		count /= 2
	}
	dst = append(dst, n.kind)
	dst = strconv.AppendInt(dst, int64(count), 10)
	dst = append(dst, '\r', '\n')
	for _, item := range n.items {
		dst = item.encode(dst)
	}
	return dst
}

// rewrite applies flag to n and everything under it; key is FlagZero's.
func (n *node) rewrite(flag, key string) {
	if !n.aggregate() {
		return
	}
	for _, item := range n.items {
		item.rewrite(flag, key)
	}
	switch flag {
	case FlagSorted:
		scalars := true
		for _, item := range n.items {
			scalars = scalars && item.scalar()
		}
		if scalars && n.kind != '%' {
			sort.SliceStable(n.items, func(i, j int) bool { return bytes.Compare(n.items[i].raw, n.items[j].raw) < 0 })
		}
	case FlagPairs:
		if len(n.items)%2 != 0 || n.kind == '~' {
			return
		}
		pairs := make([][2]*node, 0, len(n.items)/2)
		for i := 0; i < len(n.items); i += 2 {
			pairs = append(pairs, [2]*node{n.items[i], n.items[i+1]})
		}
		sort.SliceStable(pairs, func(i, j int) bool { return bytes.Compare(pairs[i][0].encode(nil), pairs[j][0].encode(nil)) < 0 })
		for i, pair := range pairs {
			n.items[2*i], n.items[2*i+1] = pair[0], pair[1]
		}
	case FlagValues:
		if len(n.items)%2 != 0 || n.kind == '~' {
			return
		}
		for i := 1; i < len(n.items); i += 2 {
			n.items[i] = &node{kind: ':', raw: []byte(":0\r\n")}
		}
	case FlagZero:
		if len(n.items)%2 != 0 || n.kind == '~' {
			return
		}
		for i := 0; i < len(n.items); i += 2 {
			if n.items[i].text() == key {
				n.items[i+1] = &node{kind: ':', raw: []byte(":0\r\n")}
			}
		}
	}
}

// text is a simple or bulk string's payload, or "" for any other value.
func (n *node) text() string {
	switch n.kind {
	case '+':
		return string(n.raw[1 : len(n.raw)-2])
	case '$':
		if n.null {
			return ""
		}
		_, payload, _ := bytes.Cut(n.raw, []byte("\r\n"))
		return string(payload[:len(payload)-2])
	}
	return ""
}
