package core

// layoutPadTestSink keeps the control's pads linked: they run only when an array
// nobody writes holds a value nobody stores.
var layoutPadTestSink [64]uint64

func init() {
	if layoutPadTestSink[63] == 0x5eed1e55 {
		layoutPadTests(&layoutPadTestSink)
	}
}
