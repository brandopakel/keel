package core

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		// The plain cases.
		{"", "", true},
		{"", "a", false},
		{"*", "", true},
		{"?", "", false},
		{"a", "a", true},
		{"a", "b", false},
		{"abc", "abc", true},

		// Stars.
		{"*", "", true},
		{"*", "anything", true},
		{"a*", "abc", true},
		{"a*", "bbc", false},
		{"*c", "abc", true},
		{"a*c", "abc", true},
		{"a*c", "abbbbbc", true},
		{"a*c", "abd", false},
		{"*a*", "bab", true},
		{"**", "ab", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxcyyb", false},

		// A star must be able to give back what it took.
		{"*abc", "xxabcxxabc", true},
		{"*ab*cd", "ababcd", true},

		// Question marks.
		{"?", "a", true},
		{"?", "", false},
		{"?", "ab", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"???", "abc", true},

		// Classes.
		{"[abc]", "b", true},
		{"[abc]", "d", false},
		{"[a-z]", "q", true},
		{"[a-z]", "Q", false},
		{"[^a]", "b", true},
		{"[^a]", "a", false},
		{"[^a-z]", "Q", true},
		{"h[ae]llo", "hello", true},
		{"h[ae]llo", "hallo", true},
		{"h[ae]llo", "hillo", false},
		{"[a-c]*", "bxx", true},
		// A range written backwards is still a range.
		{"[z-a]", "m", true},
		// Two places where Redis is not the shell, both checked against a
		// real server rather than assumed. A ']' straight after '[' closes an
		// empty class rather than being a literal member of one, so "[]]"
		// matches nothing at all. And a '-' at the end of a class is not a
		// literal either - "[a-]" matches "a" and not "-".
		{"[]]", "]", false},
		{"[a-]", "-", false},
		{"[a-]", "a", true},

		// Escapes.
		{`\*`, "*", true},
		{`\*`, "a", false},
		{`\?`, "?", true},
		{`\[`, "[", true},
		{`a\*b`, "a*b", true},
		{`a\*b`, "axb", false},
		{`\\`, `\`, true},
		// An escape inside a class.
		{`[\]]`, "]", true},

		// Keys are not paths. This is the case path.Match gets wrong, and the
		// reason this function exists.
		{"cache/*", "cache/user/1", true},
		{"*/1", "cache/user/1", true},
		{"user:*", "user:1000", true},
		{"user:*:sessions", "user:1000:sessions", true},

		// Bytes, not runes: a multi-byte character is more than one '?'.
		{"?", "é", false},
		{"??", "é", true},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, globMatch(c.pattern, c.s),
			"globMatch(%q, %q)", c.pattern, c.s)
	}
}

// TestGlobMatchAgreesWithRealRedis records that the table above is not a guess.
//
// Every case in TestGlobMatch was run against Redis 8.10.1 by setting the
// subject as a key and asking KEYS for the pattern, and the expectations here
// are that server's answers rather than the shell's or Go's path.Match's. Two
// of them contradicted what the author expected, which is the reason for
// checking rather than the reason to discard the check.
//
// This test is a marker for that provenance; the assertions live above.
func TestGlobMatchAgreesWithRealRedis(t *testing.T) {
	assert.False(t, globMatch("[]]", "]"), "']' after '[' closes an empty class")
	assert.False(t, globMatch("[a-]", "-"), "a trailing '-' is not a literal")
	assert.False(t, globMatch("?", "é"), "matching is over bytes, not runes")
}

// TestGlobMatchDoesNotBacktrackExponentially is the reason the star is a
// remembered position rather than a recursive call.
//
// The pattern below is the classic catastrophic case for a naive matcher: each
// star can split the input anywhere, and a recursive implementation tries every
// combination. This server executes commands on one thread, so a pattern that
// takes a second to match is a pattern that stalls every other client for a
// second - it is a denial of service reachable from KEYS.
func TestGlobMatchDoesNotBacktrackExponentially(t *testing.T) {
	pattern := strings.Repeat("a*", 40) + "b"
	subject := strings.Repeat("a", 200)

	done := make(chan bool, 1)
	go func() { done <- globMatch(pattern, subject) }()

	select {
	case got := <-done:
		assert.False(t, got, "no 'b' in the subject, so it cannot match")
	case <-time.After(5 * time.Second):
		t.Fatal("matching did not finish: the star is backtracking exponentially")
	}
}
