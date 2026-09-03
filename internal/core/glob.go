package core

// Redis's glob matching, for KEYS.
//
// Go has path.Match, and it is the wrong function: it refuses to let * cross a
// '/', because it matches paths. A Redis key is an arbitrary byte string and
// slashes are ordinary characters in it - "cache/user/1" is a perfectly normal
// key, and KEYS cache/* has to find it. Using path.Match would silently return
// nothing for the namespacing convention most people reach for first.
//
// Matching is over bytes rather than runes, which is also Redis's behaviour. A
// pattern is a byte string and so is a key; decoding either as UTF-8 would
// invent a failure mode for keys that are not text.

// globMatch reports whether pattern matches s.
//
//   - any sequence of bytes, including none
//     ?        any single byte
//     [abc]    any one of the bytes listed
//     [^abc]   any byte not listed
//     [a-z]    any byte in the range
//     \x       the byte x, whatever it would otherwise mean
//
// The star is handled by remembering where it was and retrying one byte later
// on a mismatch, rather than by recursing. Recursion here is how a pattern of
// alternating stars turns into exponential work on a hostile key, and this
// server runs every command on one thread - a client able to make matching take
// a second is a client able to stall every other client for a second.
func globMatch(pattern, s string) bool {
	var (
		p, i         int
		starP, starI = -1, -1
	)

	for i < len(s) {
		matched := false

		if p < len(pattern) {
			switch pattern[p] {
			case '*':
				// Remember the position to fall back to, then try to match as
				// little as possible first.
				starP, starI = p, i
				p++
				continue
			case '?':
				p++
				i++
				continue
			case '[':
				var next int
				next, matched = matchClass(pattern, p, s[i])
				if matched {
					p = next
					i++
					continue
				}
			case '\\':
				// A trailing backslash has nothing to escape, so it is itself.
				if p+1 < len(pattern) {
					if pattern[p+1] == s[i] {
						p += 2
						i++
						continue
					}
				} else if pattern[p] == s[i] {
					p++
					i++
					continue
				}
			default:
				if pattern[p] == s[i] {
					p++
					i++
					continue
				}
			}
		}

		// No match at this position. If a star is open, let it swallow one more
		// byte and try again from just after it.
		if starP >= 0 {
			starI++
			i = starI
			p = starP + 1
			continue
		}
		return false
	}

	// The key is exhausted; the pattern matches only if what is left of it can
	// match nothing at all, which is to say it is all stars.
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// matchClass matches one byte against the bracket expression starting at
// pattern[at], which is known to be '['. It returns the index just past the
// closing ']' and whether c matched.
//
// An unterminated class - "[abc" - stops at the end of the pattern rather than
// being an error, which is what Redis does. Refusing it would be defensible,
// but not while claiming to accept Redis's patterns.
func matchClass(pattern string, at int, c byte) (int, bool) {
	p := at + 1

	negate := p < len(pattern) && pattern[p] == '^'
	if negate {
		p++
	}

	match := false
	for {
		switch {
		case p >= len(pattern):
			// Unterminated. Everything seen so far still counts.
			if negate {
				match = !match
			}
			return p, match

		case pattern[p] == '\\' && p+1 < len(pattern):
			p++
			if pattern[p] == c {
				match = true
			}

		case pattern[p] == ']':
			p++
			if negate {
				match = !match
			}
			return p, match

		// A range is any three bytes with a '-' in the middle, and the third
		// is not exempted for being ']'. That is what makes "[a-]" match "a"
		// and not "-": Redis reads it as the range 'a' to ']', swaps the ends
		// because they are backwards, and never reaches a closing bracket. The
		// shell would call that trailing '-' a literal. Checked against Redis
		// 8.10.1 rather than reasoned about, because the two disagree.
		case p+2 < len(pattern) && pattern[p+1] == '-':
			lo, hi := pattern[p], pattern[p+2]
			if lo > hi {
				lo, hi = hi, lo
			}
			if c >= lo && c <= hi {
				match = true
			}
			p += 2

		default:
			if pattern[p] == c {
				match = true
			}
		}
		p++
	}
}
