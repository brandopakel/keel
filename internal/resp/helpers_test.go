package resp

// appendCommand writes one command as a client would send it: an array of
// bulk strings.
func appendCommand(dst []byte, parts ...string) []byte {
	dst = AppendArrayHeader(dst, len(parts))
	for _, p := range parts {
		dst = AppendBulkString(dst, p)
	}
	return dst
}
