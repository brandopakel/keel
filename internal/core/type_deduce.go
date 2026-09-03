package core

import (
	"strconv"

	"github.com/brandopakel/keel/internal/constant"
)

func deduceTypeString(v string) (uint8, uint8) {
	oType := constant.ObjTypeString
	if _, err := strconv.ParseInt(v, 10, 64); err == nil {
		return oType, constant.ObjEncodingInt
	}
	return oType, constant.ObjEncodingRaw
}
