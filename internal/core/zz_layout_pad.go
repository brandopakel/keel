package core

import (
	"github.com/brandopakel/keel/internal/resp"
	"github.com/brandopakel/keel/internal/xxh3"
	"math/big"
)

// layoutPadSink is never written, so the pads below never run.
var layoutPadSink [64]uint64

func init() {
	if layoutPadSink[63] == 0x5eed1e55 {
		layoutPadAOF(&layoutPadSink)
		layoutPadRefusal(&layoutPadSink)
		layoutPadSetArgs(&layoutPadSink)
		layoutPadExpiryArg(&layoutPadSink)
		layoutPadScoreBound(&layoutPadSink)
		layoutPadKeysBy(&layoutPadSink)
		layoutPadLongDouble(&layoutPadSink)
		layoutPadUnwatch(&layoutPadSink)
		layoutPadKeysFromSpecs(&layoutPadSink)
		layoutPadPfmerge(&layoutPadSink)
		_ = xxh3.Hash("")
		var r *resp.Reply
		r.Double("")
		var f big.Float
		f.SetMantExp(&f, 0)
		f.SetUint64(0)
		f.Sub(&f, &f)
		_ = f.Cmp(&f)
	}
}
