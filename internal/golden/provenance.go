package golden

import (
	"fmt"
	"os"
	"regexp"
	"time"
)

// CaptureRevisionEnv names the variable scripts/capture-command-goldens.py
// sets, for a capture test, to the revision being captured: HEAD, in the
// form `git rev-parse --short=7` prints, refused while what a command
// answers differs from HEAD in the working tree.
const CaptureRevisionEnv = "KEEL_CAPTURE_REVISION"

// Provenance is where a fixture file's bytes came from: the revision they
// were captured at, short, and the day, in UTC. Each file of a corpus
// carries its own in the corpus's manifest.json, in place of a revision in
// the fixture directory's name, so that the directory keeps one name and a
// change that recaptures one family's file changes that file and its entry
// only. Two branches that add cases to different families then merge
// without a conflict, and only branches that change the same file have to
// recapture after the other merges.
type Provenance struct {
	SourceRevision string `json:"source_revision"`
	Captured       string `json:"captured"`
}

var shortRevision = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// Check returns what is wrong with p: a revision that is not a commit's hash
// in hex, of at least seven digits, or a day that is not YYYY-MM-DD.
func (p Provenance) Check() error {
	if !shortRevision.MatchString(p.SourceRevision) {
		return fmt.Errorf("source_revision %q is not a revision's hash, of 7 to 40 hex digits", p.SourceRevision)
	}
	if _, err := time.Parse(time.DateOnly, p.Captured); err != nil {
		return fmt.Errorf("captured %q is not a day, YYYY-MM-DD", p.Captured)
	}
	return nil
}

// CaptureProvenance is the provenance of a file a capture writes now: the
// revision CaptureRevisionEnv names, and today.
func CaptureProvenance() (Provenance, error) {
	p := Provenance{SourceRevision: os.Getenv(CaptureRevisionEnv), Captured: time.Now().UTC().Format(time.DateOnly)}
	if err := p.Check(); err != nil {
		return Provenance{}, fmt.Errorf("%s: %w; capture through scripts/capture-command-goldens.py, which sets it", CaptureRevisionEnv, err)
	}
	return p, nil
}

// Carry is the provenance a capture writes for a file whose digest is
// sha256: what the fixture held for the file, when the fixture held these
// same bytes under a sound provenance, and otherwise fresh. A file the
// capture did not change keeps its entry, so a recapture's diff is the
// files that changed and their entries, and nothing else.
func Carry(held Provenance, heldSHA256, sha256 string, fresh Provenance) Provenance {
	if heldSHA256 == sha256 && held.Check() == nil {
		return held
	}
	return fresh
}
