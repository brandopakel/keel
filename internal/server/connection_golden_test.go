package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/golden"
)

// The connection corpus: what the server answers for the commands it
// answers itself - AUTH, HELLO, QUIT and CLIENT - and for a command refused
// before it reaches the engine, on a connection that has not logged in,
// held to what develop answered at the revision its manifest names. It is
// the server's counterpart of internal/core's command corpus
// (command_golden_test.go), in the same fixture format (package golden),
// with the same manifest, in a directory whose name does not change, and the
// same script captures both.
//
// Each case is a session on a connection of its own, to an engine of its own
// with an empty keyspace, run through the server's own execution of a run of
// commands (executeRun), once on a connection that starts in RESP2 and once
// on one that starts in RESP3, as HELLO 3 would have left it. A HELLO inside
// the session switches the connection as it would a client's. The engine
// keeps no log here: what the data commands write is the command corpus's.
const connectionGoldenDir = "testdata/connection"

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
			if len(args) > 1 && strings.EqualFold(args[0], "CLIENT") &&
				(strings.EqualFold(args[1], "INFO") || strings.EqualFold(args[1], "LIST")) {
				reply = steadyClientLines(reply)
			}
			if i == 0 {
				out.Steps = append(out.Steps, golden.Step{Args: slices.Clone(args)})
			}
			out.Steps[j].Replies[i] = golden.Held(reply)
		}
	}
	return out
}

// clientLineClock matches the fields of a CLIENT INFO or LIST line that
// depend on the clock or on the build rather than on the session: how long
// the connection has been open and idle, and the size of its own record.
var clientLineClock = regexp.MustCompile(`\b(age|idle|tot-mem)=[0-9]+\b`)

// steadyClientLines is reply, a CLIENT INFO or LIST reply in either protocol,
// with clientLineClock's fields 0 and the length it is framed with counted
// again.
func steadyClientLines(reply string) string {
	header, body, ok := strings.Cut(reply, "\r\n")
	if !ok || len(header) < 2 || (header[0] != '$' && header[0] != '=') {
		return reply
	}
	body = strings.TrimSuffix(body, "\r\n")
	body = clientLineClock.ReplaceAllString(body, "${1}=0")
	return fmt.Sprintf("%c%d\r\n%s\r\n", header[0], len(body), body)
}

// connectionGoldenManifest is the fixture's manifest: for each file, its
// digest and counts, and the revision and day it was captured
// (golden.Provenance).
type connectionGoldenManifest struct {
	Note  string                                   `json:"note"`
	Files map[string]connectionGoldenManifestEntry `json:"files"`
}

type connectionGoldenManifestEntry struct {
	SHA256   string `json:"sha256"`
	Cases    int    `json:"cases"`
	Commands int    `json:"commands"`
	golden.Provenance
}

// readConnectionGoldenManifest reads the fixture's manifest.
func readConnectionGoldenManifest(dir string) (connectionGoldenManifest, error) {
	var manifest connectionGoldenManifest
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return manifest, err
	}
	return manifest, json.Unmarshal(body, &manifest)
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
	fresh, err := golden.CaptureProvenance()
	require.NoError(t, err)
	held, err := readConnectionGoldenManifest(connectionGoldenDir)
	if !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err, "the fixture's manifest")
	}
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
	entry := connectionGoldenManifestEntry{SHA256: hex.EncodeToString(sum[:]), Cases: len(cases), Commands: commands}
	was := held.Files["connection.txt"]
	entry.Provenance = golden.Carry(was.Provenance, was.SHA256, entry.SHA256, fresh)
	manifest := connectionGoldenManifest{
		Note:  "The connection commands' RESP2 and RESP3 replies, as captured at the revision the entry names; see connection_golden_test.go.",
		Files: map[string]connectionGoldenManifestEntry{"connection.txt": entry},
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

// loadConnectionGoldens reads the fixture, checked against its manifest,
// with a sound provenance for each file.
func loadConnectionGoldens(t *testing.T) map[string]golden.Case {
	t.Helper()
	manifest, err := readConnectionGoldenManifest(connectionGoldenDir)
	require.NoError(t, err, "the fixture's manifest")
	cases := map[string]golden.Case{}
	for name, entry := range manifest.Files {
		require.NoError(t, entry.Check(), "%s: the manifest's provenance", name)
		body, err := os.ReadFile(filepath.Join(connectionGoldenDir, name))
		require.NoError(t, err)
		sum := sha256.Sum256(body)
		require.Equal(t, entry.SHA256, hex.EncodeToString(sum[:]), "%s: the fixture's bytes", name)
		parsed, err := golden.Parse(strings.NewReader(string(body)))
		require.NoError(t, err, name)
		commands := 0
		for _, c := range parsed {
			_, dup := cases[c.Name]
			require.False(t, dup, "%s holds %s twice", name, c.Name)
			cases[c.Name] = c
			commands += len(c.Steps)
		}
		require.Equal(t, entry.Cases, len(parsed), "%s: the manifest's count of cases", name)
		require.Equal(t, entry.Commands, commands, "%s: the manifest's count of commands", name)
	}
	return cases
}

// TestConnectionGoldenRepliesAreUnchanged: every case answers, on a
// connection starting in either protocol, what develop answered at the
// revision the fixture was captured at.
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
var uncoveredConnectionErrorTexts = map[string]string{
	"ERR can only reset normal client connections": "RESET from a replica's connection, which the corpus's connections never are",
}

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
