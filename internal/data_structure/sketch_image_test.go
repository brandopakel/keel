package data_structure

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSketchImageRangesPreserveExistingEncoding(t *testing.T) {
	cms, morris := CreateCMS(37, 3), CreateMorris(37, 3)
	cms.IncrBy("item", 0x01020304)
	morris.IncrBy("item", 12345)
	for _, tc := range []struct {
		name  string
		image *SketchImage
		want  []byte
	}{{"cms", cms.RewriteImage(), cms.Marshal()}, {"morris", morris.RewriteImage(), morris.Marshal()}} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, len(tc.want), tc.image.Size())
			for offset := 0; offset <= len(tc.want); offset++ {
				for _, count := range []int{0, 1, 2, 3, 4, 5, 16, 23, 24, 64, 65536} {
					got := tc.image.AppendRange([]byte("prefix"), offset, count)
					require.Equal(t, append([]byte("prefix"), tc.want[offset:min(len(tc.want), offset+count)]...), got)
				}
			}
			var got []byte
			for offset := 0; offset < tc.image.Size(); offset += 3 {
				got = tc.image.AppendRange(got, offset, 3)
			}
			require.True(t, bytes.Equal(tc.want, got))
			require.Equal(t, []byte("prefix"), tc.image.AppendRange([]byte("prefix"), -1, 3))
		})
	}
}
