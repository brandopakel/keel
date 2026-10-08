package core

// layoutPad is part of a layout control, never merged: it is never called,
// and only moves the code after it by one 32-byte slot.
//
//go:noinline
func layoutPad() {}
