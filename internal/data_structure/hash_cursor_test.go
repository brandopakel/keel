package data_structure

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHashCursorVisitsEachPairAndHandlesEmptyHash(t *testing.T) {
	h := NewHash()
	_, _, ok := h.Cursor().Entry()
	require.False(t, ok)
	want := map[string]string{}
	for n := 0; n < 1000; n++ {
		field, value := strconv.Itoa(n), strconv.Itoa(n*7)
		h.Set(field, value)
		want[field] = value
	}
	got := map[string]string{}
	c := h.Cursor()
	for field, value, ok := c.Entry(); ok; field, value, ok = c.Entry() {
		_, duplicate := got[field]
		require.False(t, duplicate)
		got[field] = value
		c.Advance()
	}
	require.Equal(t, want, got)
}

var hashTraversalBytes int

func BenchmarkHashTraversal(b *testing.B) {
	h := NewHash()
	for n := 0; n < 100000; n++ {
		h.Set(strconv.Itoa(n), "value")
	}
	b.Run("materialized", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			fields, values := h.Entries()
			for i, f := range fields {
				hashTraversalBytes += len(f) + len(values[i])
			}
		}
	})
	b.Run("cursor", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			c := h.Cursor()
			for field, value, ok := c.Entry(); ok; field, value, ok = c.Entry() {
				hashTraversalBytes += len(field) + len(value)
				c.Advance()
			}
		}
	})
}
