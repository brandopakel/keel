package constant

var RespNil = []byte("$-1\r\n")

// RespNilArray is the null array, which is a different reply from the null
// bulk string above. A command that answers an array has to answer this when it
// has no array to give - a client decoding LPOP key count into a list gets a
// type error from $-1, because that is the absence of a string.
var RespNilArray = []byte("*-1\r\n")
var RespOk = []byte("+OK\r\n")
var RespZero = []byte(":0\r\n")
var RespOne = []byte(":1\r\n")
var RespEmptyArray = []byte("*0\r\n")
var TtlKeyNotExist = []byte(":-2\r\n")
var TtlKeyExistNoExpire = []byte(":-1\r\n")

const NoExpire int64 = -1

const (
	ObjTypeString  uint8 = 0
	ObjTypeSet     uint8 = 1
	ObjTypeZSet    uint8 = 2
	ObjTypeGeoHash uint8 = 3
)

const ObjEncodingRaw uint8 = 0
const ObjEncodingInt uint8 = 1
