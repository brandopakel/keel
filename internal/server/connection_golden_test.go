package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/golden"
)

// The connection corpus: what the server answers for the commands it
// answers itself - AUTH, HELLO, QUIT and CLIENT - and for a command refused
// before it reaches the engine, on a connection that has not logged in,
// held to what develop answered at connectionGoldenRevision. It is the
// server's counterpart of internal/core's command corpus
// (command_golden_test.go), in the same fixture format (package golden), and
// the same script captures both.
//
// Each case is a session on a connection of its own, to an engine of its own
// with an empty keyspace, run through the server's own execution of a run of
// commands (executeRun), once on a connection that starts in RESP2 and once
// on one that starts in RESP3, as HELLO 3 would have left it. A HELLO inside
// the session switches the connection as it would a client's. The engine
// keeps no log here: what the data commands write is the command corpus's.
const (
	connectionGoldenRevision = "5ae6dbf"
	connectionGoldenDir      = "testdata/connection-" + connectionGoldenRevision
)

// connectionGoldenCase is one session.
type connectionGoldenCase struct {
	name string
	// password is the server's RequirePass; empty, none is needed.
	password string
	commands [][]string
}

// runConnectionGoldenCase runs cs on a connection starting in each
// protocol, and returns the case with both replies beside each command.
func runConnectionGoldenCase(t *testing.T, cs connectionGoldenCase) golden.Case {
	t.Helper()
	out := golden.Case{Name: "connection/" + cs.name}
	for i, resp3 := range []bool{false, true} {
		c := &client{fd: -1, password: cs.password, resp3: resp3, engine: newTestEngine(t, core.Options{})}
		for j, args := range cs.commands {
			reply := runOnce(t, c, sent(args...))
			if i == 0 {
				out.Steps = append(out.Steps, golden.Step{Args: slices.Clone(args)})
			}
			out.Steps[j].Replies[i] = golden.Held(reply)
		}
	}
	return out
}

type connectionGoldenManifest struct {
	SourceRevision string            `json:"source_revision"`
	Captured       string            `json:"captured"`
	Note           string            `json:"note"`
	Files          map[string]string `json:"files"`
	Cases          int               `json:"cases"`
	Commands       int               `json:"commands"`
}

// TestConnectionGoldenCapture writes the fixture when asked to, and is
// skipped otherwise; scripts/capture-command-goldens.py runs it. A case is
// run three times first, and refused if its replies differ between runs.
func TestConnectionGoldenCapture(t *testing.T) {
	dir := os.Getenv("KEEL_CAPTURE_COMMAND_GOLDEN")
	if dir == "" {
		t.Skip("set KEEL_CAPTURE_COMMAND_GOLDEN to write the fixture")
	}
	dir = filepath.Join(dir, "server")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	var cases []golden.Case
	commands := 0
	for _, cs := range connectionGoldenCorpus {
		first := runConnectionGoldenCase(t, cs)
		for i := 0; i < 2; i++ {
			again := runConnectionGoldenCase(t, cs)
			if !slices.EqualFunc(first.Steps, again.Steps, func(a, b golden.Step) bool {
				return slices.Equal(a.Args, b.Args) && a.Replies == b.Replies
			}) {
				explainConnectionGoldenDifference(t, first, again)
				t.Fatalf("%s is not the same from run to run", first.Name)
			}
		}
		cases = append(cases, first)
		commands += len(first.Steps)
	}
	var body strings.Builder
	require.NoError(t, golden.Write(&body, cases))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "connection.txt"), []byte(body.String()), 0o644))
	sum := sha256.Sum256([]byte(body.String()))
	manifest := connectionGoldenManifest{
		SourceRevision: connectionGoldenRevision,
		Captured:       time.Now().UTC().Format("2006-01-02"),
		Note:           "The connection commands' RESP2 and RESP3 replies on develop, before phase 4 of the embedding plan; see connection_golden_test.go.",
		Files:          map[string]string{"connection.txt": hex.EncodeToString(sum[:])},
		Cases:          len(cases), Commands: commands,
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), append(encoded, '\n'), 0o644))
}

func explainConnectionGoldenDifference(t *testing.T, want, got golden.Case) {
	t.Helper()
	for i := range min(len(want.Steps), len(got.Steps)) {
		w, g := want.Steps[i], got.Steps[i]
		at := fmt.Sprintf("%s: step %d of %d", want.Name, i+1, len(want.Steps))
		switch {
		case !slices.Equal(w.Args, g.Args):
			t.Errorf("%s: command %q, want %q", at, g.Args, w.Args)
		case w.Replies[0] != g.Replies[0]:
			t.Errorf("%s %q: on a RESP2 connection\n got %q\nwant %q", at, g.Args, g.Replies[0], w.Replies[0])
		case w.Replies[1] != g.Replies[1]:
			t.Errorf("%s %q: on a RESP3 connection\n got %q\nwant %q", at, g.Args, g.Replies[1], w.Replies[1])
		default:
			continue
		}
		return
	}
	if len(want.Steps) != len(got.Steps) {
		t.Errorf("%s: %d steps, want %d", want.Name, len(got.Steps), len(want.Steps))
	}
}

// loadConnectionGoldens reads the fixture, checked against its manifest.
func loadConnectionGoldens(t *testing.T) map[string]golden.Case {
	t.Helper()
	var manifest connectionGoldenManifest
	body, err := os.ReadFile(filepath.Join(connectionGoldenDir, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &manifest))
	require.Equal(t, connectionGoldenRevision, manifest.SourceRevision, "the fixture's revision")
	cases := map[string]golden.Case{}
	commands := 0
	for name, want := range manifest.Files {
		body, err := os.ReadFile(filepath.Join(connectionGoldenDir, name))
		require.NoError(t, err)
		sum := sha256.Sum256(body)
		require.Equal(t, want, hex.EncodeToString(sum[:]), "%s: the fixture's bytes", name)
		parsed, err := golden.Parse(strings.NewReader(string(body)))
		require.NoError(t, err, name)
		for _, c := range parsed {
			_, dup := cases[c.Name]
			require.False(t, dup, "%s holds %s twice", name, c.Name)
			cases[c.Name] = c
			commands += len(c.Steps)
		}
	}
	require.Equal(t, manifest.Cases, len(cases), "the manifest's count of cases")
	require.Equal(t, manifest.Commands, commands, "the manifest's count of commands")
	return cases
}

// TestConnectionGoldenRepliesAreUnchanged: every case answers, on a
// connection starting in either protocol, what develop answered at the
// fixture's revision.
func TestConnectionGoldenRepliesAreUnchanged(t *testing.T) {
	if os.Getenv("KEEL_CAPTURE_COMMAND_GOLDEN") != "" {
		t.Skip("capturing")
	}
	fixture := loadConnectionGoldens(t)
	named := map[string]bool{}
	for _, cs := range connectionGoldenCorpus {
		name := "connection/" + cs.name
		require.False(t, named[name], "the corpus holds %s twice", name)
		named[name] = true
		want, ok := fixture[name]
		require.True(t, ok, "%s is in the corpus and not in the fixture: capture it", name)
		got := runConnectionGoldenCase(t, cs)
		explainConnectionGoldenDifference(t, want, got)
	}
	for name := range fixture {
		require.True(t, named[name], "%s is in the fixture and not in the corpus", name)
	}
}

// uncoveredConnectionErrorTexts are the error texts the server holds that no
// case of the connection corpus can reach, each with the reason.
var uncoveredConnectionErrorTexts = map[string]string{}

// TestConnectionGoldensCoverEveryErrorText: every error text the server can
// answer with (golden.ErrorTexts) is answered somewhere in the fixture,
// apart from those listed above.
func TestConnectionGoldensCoverEveryErrorText(t *testing.T) {
	if os.Getenv("KEEL_CAPTURE_COMMAND_GOLDEN") != "" {
		t.Skip("capturing")
	}
	fixture := loadConnectionGoldens(t)
	texts, err := golden.ErrorTexts(".")
	require.NoError(t, err)
	require.NotEmpty(t, texts)
	for _, problem := range golden.Uncovered(texts, golden.ReplyTexts(fixture), uncoveredConnectionErrorTexts) {
		t.Error(problem)
	}
}
