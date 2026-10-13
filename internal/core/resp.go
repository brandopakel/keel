package core

import "github.com/brandopakel/keel/internal/resp"

// The protocol lives in internal/resp (phase 4 of docs/embedding-plan.md):
// the parser that turns bytes into a Command, and the encoders that frame a
// reply in RESP2 or RESP3. What is here is the names this package and the
// server have always used for it, each a wrapper the compiler inlines, so
// that a command's path is the same instructions it was and no handler
// changed for the move. A caller outside this package may use resp directly,
// and these wrappers go once the last one does.

// Command is one parsed request: the name, upper-cased, and its arguments.
type Command = resp.Command

// parsedCommand is a request whose name arrived as name: upper-cased into
// Cmd, and kept as sent when upper-casing changed it.
func parsedCommand(name string, args []string) *Command { return resp.ParsedCommand(name, args) }

const CRLF = resp.CRLF

var (
	// ErrIncompleteFrame means data holds the start of a well-formed RESP
	// value but not all of it yet; the caller reads more and tries again.
	ErrIncompleteFrame = resp.ErrIncompleteFrame
	// ErrProtocol means data cannot become a valid RESP value no matter how
	// many more bytes arrive.
	ErrProtocol = resp.ErrProtocol
	// ErrRequestAllocation refuses a request the transport's budget cannot
	// hold.
	ErrRequestAllocation = resp.ErrRequestAllocation
)

// Upper bounds borrowed from Redis, which the log's reader holds a record to
// as the parser holds a request.
const (
	maxMultiBulkLength = resp.MaxMultiBulkLength
	maxBulkLength      = resp.MaxBulkLength
)

// parseDecimal reads an optionally signed decimal integer without allocating.
func parseDecimal(b []byte) (int64, bool) { return resp.ParseDecimal(b) }

// DecodeOne decodes the single RESP value at the front of data and reports
// how many bytes it consumed.
func DecodeOne(data []byte) (interface{}, int, error) { return resp.DecodeOne(data) }

// Decode decodes the value at the front of data, ignoring anything after it.
func Decode(data []byte) (interface{}, error) { return resp.Decode(data) }

// ParseCmd decodes the first complete command in data and reports how many
// bytes it consumed.
func ParseCmd(data []byte) (*Command, int, error) { return resp.ParseCmd(data) }

// ParseCmdReserved validates one complete frame before reserving and copying
// its strings; see resp.ParseCmdReserved.
func ParseCmdReserved(data []byte, reserve func(int) bool) (*Command, int, error) {
	return resp.ParseCmdReserved(data, reserve)
}

// FrameShortfall reports how many more bytes must arrive before the command
// at the front of data can be decoded, or 0 when that is not knowable.
func FrameShortfall(data []byte) int { return resp.FrameShortfall(data) }

// RequestAllocationSize is the admission charge for an owned allocation of n
// bytes, with its size-class headroom.
func RequestAllocationSize(n int) (int, bool) { return resp.RequestAllocationSize(n) }

// The append helpers build RESP into a caller-supplied slice; they write only
// what both protocols share, which is why the log and the replication feed
// are built from them alone.
func appendBulkString(dst []byte, s string) []byte   { return resp.AppendBulkString(dst, s) }
func appendArrayHeader(dst []byte, n int) []byte     { return resp.AppendArrayHeader(dst, n) }
func appendSimpleString(dst []byte, s string) []byte { return resp.AppendSimpleString(dst, s) }
func appendError(dst []byte, msg string) []byte      { return resp.AppendError(dst, msg) }
func appendInteger(dst []byte, n int64) []byte       { return resp.AppendInteger(dst, n) }
func encodeString(s string) []byte                   { return resp.EncodeString(s) }
func encodeStringArray(sa []string) []byte           { return resp.EncodeStringArray(sa) }

// Encode builds the reply for a Go value, framed in RESP2, for a reply built
// outside any command. A handler encodes through the engine running it,
// e.encode, which frames the reply in the protocol of the command.
func Encode(value interface{}, isSimpleString bool) []byte { return resp.Encode(value, isSimpleString) }

// EncodeAs is Encode in the protocol given rather than RESP2, for the
// connection layer, which knows its connection's protocol.
func EncodeAs(value interface{}, isSimpleString, resp3 bool) []byte {
	return resp.EncodeAs(value, isSimpleString, resp3)
}

// The reply values Encode frames differently for the two protocols.
type (
	// ReplyMap is keys and values alternating: a RESP3 map, and in RESP2
	// the flat array RESP2 clients decode into a map.
	ReplyMap = resp.Map
	// ReplyDouble is a float already written as text: a RESP3 double, and
	// a bulk string in RESP2.
	ReplyDouble = resp.Double
	// ReplyBool is a yes-or-no: a RESP3 boolean, and 1 or 0 in RESP2.
	ReplyBool = resp.Bool
	// ReplyVerbatim is text shown as it is, INFO's: a RESP3 verbatim string
	// of format txt, and a bulk string in RESP2.
	ReplyVerbatim = resp.Verbatim
	// infoField is a field name in BF.INFO and CF.INFO, a simple string in
	// both protocols.
	infoField = resp.Field
)

// MaxReplyBytes is the per-client encoded output ceiling. Amplifying reads
// enforce it before allocating their payload, independently of append mode.
const MaxReplyBytes = resp.MaxReplyBytes

func decimalDigits(n int) int { return resp.DecimalDigits(n) }

// addBulkSize checks before adding, including RESP framing, without
// overflowing.
func addBulkSize(size, length int) (int, bool) { return resp.AddBulkSize(size, length) }
