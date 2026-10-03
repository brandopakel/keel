// Package constant holds the replies every command hands back ready-made, and
// the type and encoding tags a stored string carries.
package constant

// Canned RESP replies. Each is encoded once here rather than on every reply,
// and handed back by reference, so a command that answers OK or 0 allocates
// nothing. Nothing may append to these: they are shared.
var (
	RespOk         = []byte("+OK\r\n")
	RespZero       = []byte(":0\r\n")
	RespOne        = []byte(":1\r\n")
	RespNil        = []byte("$-1\r\n")
	RespNilArray   = []byte("*-1\r\n")
	RespEmptyArray = []byte("*0\r\n")

	// TTL's two sentinels: the key does not exist, and it exists with no
	// expiry.
	TtlKeyNotExist      = []byte(":-2\r\n")
	TtlKeyExistNoExpire = []byte(":-1\r\n")

	// The RESP3 forms a connection that negotiated HELLO 3 is answered with
	// where RESP2 has another. RESP3 has one null for both of RESP2's, and
	// types of its own for an empty map, an empty set and a boolean.
	Resp3Null     = []byte("_\r\n")
	Resp3EmptyMap = []byte("%0\r\n")
	Resp3EmptySet = []byte("~0\r\n")
	Resp3True     = []byte("#t\r\n")
	Resp3False    = []byte("#f\r\n")
)

// A stored string carries one byte saying what it is and how it is held: the
// type in the high four bits, the encoding in the low four. Only one type
// lives in the string keyspace - every other type has a keyspace of its own -
// but the encoding matters: a string that holds a canonical integer is marked
// so that INCR can act on it without parsing first.
const (
	ObjTypeString uint8 = 0 << 4

	ObjEncodingRaw uint8 = 0
	ObjEncodingInt uint8 = 1
)
