package core

// layoutPadSink keeps the control's pads linked: they run only when an array
// nobody writes holds a value nobody stores.
var layoutPadSink [64]uint64

func init() {
	if layoutPadSink[63] == 0x5eed1e55 {
		layoutPadAOF(&layoutPadSink)
		layoutPadReadSave(&layoutPadSink)
		layoutPadInfo(&layoutPadSink)
		layoutPadWithDefaults(&layoutPadSink)
		layoutPadLimits(&layoutPadSink)
		layoutPadFinish(&layoutPadSink)
		layoutPadAck(&layoutPadSink)
	}
}
