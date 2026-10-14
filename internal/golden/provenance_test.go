package golden

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProvenanceCheck(t *testing.T) {
	t.Parallel()
	require.NoError(t, Provenance{SourceRevision: "67aa4e2", Captured: "2026-10-14"}.Check())
	require.NoError(t, Provenance{SourceRevision: "67aa4e2b1c", Captured: "2026-10-14"}.Check())
	for _, p := range []Provenance{
		{},
		{SourceRevision: "67aa4e", Captured: "2026-10-14"},
		{SourceRevision: "67AA4E2", Captured: "2026-10-14"},
		{SourceRevision: "HEAD", Captured: "2026-10-14"},
		{SourceRevision: "67aa4e2", Captured: ""},
		{SourceRevision: "67aa4e2", Captured: "2026-13-01"},
		{SourceRevision: "67aa4e2", Captured: "14 Oct 2026"},
	} {
		require.Error(t, p.Check(), "%+v", p)
	}
}

func TestCaptureProvenanceNamesTheRevisionAndToday(t *testing.T) {
	t.Setenv(CaptureRevisionEnv, "")
	_, err := CaptureProvenance()
	require.ErrorContains(t, err, CaptureRevisionEnv)
	t.Setenv(CaptureRevisionEnv, "abc1234")
	p, err := CaptureProvenance()
	require.NoError(t, err)
	require.Equal(t, "abc1234", p.SourceRevision)
	day, err := time.Parse(time.DateOnly, p.Captured)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().UTC(), day, 48*time.Hour)
}

func TestCarryKeepsTheEntryOfAnUnchangedFile(t *testing.T) {
	t.Parallel()
	held := Provenance{SourceRevision: "1111111", Captured: "2026-10-01"}
	fresh := Provenance{SourceRevision: "2222222", Captured: "2026-10-14"}
	require.Equal(t, held, Carry(held, "aa", "aa", fresh), "the same bytes keep what the fixture held")
	require.Equal(t, fresh, Carry(held, "aa", "bb", fresh), "changed bytes are this capture's")
	require.Equal(t, fresh, Carry(Provenance{}, "", "bb", fresh), "a new file is this capture's")
	require.Equal(t, fresh, Carry(Provenance{}, "aa", "aa", fresh), "an unsound entry is replaced")
}
