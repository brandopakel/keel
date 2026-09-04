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
