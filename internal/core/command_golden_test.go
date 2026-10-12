package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/golden"
)

// The command corpus: what every command this package runs answers, on a
// RESP2 and on a RESP3 connection, and what it writes to the log, held to
// what develop answered and wrote at commandGoldenRevision.
//
// Phase 4 of the embedding plan (docs/embedding-plan.md) rewrites how a
// command is parsed, run and answered - the reply encoders into a package of
// their own, the dispatch tables into one descriptor table, each family into
// typed operations - and none of that may change a byte a client or a log
// sees. This corpus is the gate: every command Keel has, with the arguments
// that reach each of its paths and every error its handler produces, and the
// records each write command stages, captured once from develop and compared
// byte for byte on every run since. The persistence goldens
// (persistence_golden_test.go) hold the log's bytes through flushes, rewrites
// and transactions; this corpus holds each command's own answer and records,
// command by command, in both protocols, so that a difference names the
// command and the byte.
//
// The corpus is command_golden_corpus_test.go, a session per case, each on an
// engine of its own with an empty keyspace and the log open under fsync no.
// A case runs twice, as a RESP2 connection and as a RESP3 one, and the
// fixture holds both replies beside each command; the log's records have to
// be the same bytes either way, and are held once. What is not the same from
// run to run is normalized, and the fixture says how at each command (see
// package golden): a set's members are sorted, a hash's pairs are sorted by
// field, a TTL is rounded to the hour, and a reply whose content is the
// clock's or the machine's is held to its type byte. A relative expiry's
// record is normalized as the persistence goldens normalize one.
//
// testdata/commands-<revision>/ holds one file per family and a manifest.
// scripts/capture-command-goldens.py writes it, by running
// TestCommandGoldenCapture with KEEL_CAPTURE_COMMAND_GOLDEN naming a
// directory; the capture runs every case three times in each protocol and
// refuses to write a case whose replies or records differ between runs. Two
// tests read the fixture on every run: TestCommandGoldenRepliesAreUnchanged
// replays every case and compares each command's replies and records, and
// command_golden_coverage_test.go checks that the corpus names every command
// and reaches every error text the handlers hold.
const (
	commandGoldenRevision = "72ed0cd"
	commandGoldenDir      = "testdata/commands-" + commandGoldenRevision
)

// goldenStep is one step of a case: a command, with the normalizers its
// reply and records take, or a directive run on the harness between
// commands.
type goldenStep struct {
	args  []string
	flags []string
	// directive names a step that is not a command, and do runs it.
	directive string
	do        func(r *commandGoldenRun)
}

// commandGoldenCase is one session of the corpus.
type commandGoldenCase struct {
	family, name string
	// options adjusts the engine the case runs on, from the harness's
	// defaults: a replica, a memory bound, a smaller LCS budget.
	options func(*Options)
	// noLog runs the case with no log open, for what a command answers then.
	noLog bool
	// noRecords leaves the log's records out of the comparison: a rewrite
	// replaces the file under the harness.
	noRecords bool
	steps     []goldenStep
}

func (c commandGoldenCase) fullName() string { return c.family + "/" + c.name }

// send is a command of a case, as a client sends it.
func send(args ...string) goldenStep { return goldenStep{args: args} }

// norm is the step with normalizers added to its reply.
func (s goldenStep) norm(flags ...string) goldenStep {
	s.flags = append(slices.Clone(s.flags), flags...)
	return s
}

// The directives.

func sleepStep(d time.Duration) goldenStep {
	return goldenStep{directive: "sleep " + d.String(), do: func(*commandGoldenRun) { time.Sleep(d) }}
}

// expireCycleStep runs the expiry cycle until it reaps something, so a key
// whose TTL has passed is removed by active expiry rather than by a read.
var expireCycleStep = goldenStep{directive: "expire-cycle", do: func(r *commandGoldenRun) {
	for i := 0; r.e.ExpireCycle() == 0; i++ {
		require.Less(r.t, i, 1000, "the expiry cycle reaped nothing")
	}
	r.flush()
}}

// budgetStep installs an allocation budget of n bytes on the engine, as the
// server installs its own, so that a command that reserves more is refused.
func budgetStep(n int) goldenStep {
	return goldenStep{directive: "budget " + strconv.Itoa(n), do: func(r *commandGoldenRun) {
		r.e.SetCommandAllocations(&CommandAllocationBudget{Limit: n})
		r.e.CommandAllocations().Begin(0)
	}}
}

// ceilingStep bounds the next replies at n encoded bytes, as EXEC bounds the
// replies of the commands it runs, so that a larger reply is refused.
func ceilingStep(n int) goldenStep {
	return goldenStep{directive: "reply-ceiling " + strconv.Itoa(n), do: func(r *commandGoldenRun) { r.e.replyCeiling = n }}
}

// failLogStep makes every write of the log fail, with a cause of the
// harness's, and healLogStep lets writes through again.
var failLogStep = goldenStep{directive: "fail-log", do: func(r *commandGoldenRun) {
	r.e.aofWrite = func(*os.File, []byte) (int, error) { return 0, errors.New("the harness refused the write") }
}}

var healLogStep = goldenStep{directive: "heal-log", do: func(r *commandGoldenRun) { r.e.aofWrite = writeLog }}

// loadTermStep gives the engine a term file beside its log, as the server's
// startup does, so that KEEL.PROMOTE and KEEL.FENCE have somewhere to write.
var loadTermStep = goldenStep{directive: "load-term", do: func(r *commandGoldenRun) {
	require.NoError(r.t, r.e.LoadTerm(r.path))
}}

// finishRewriteStep drives a rewrite the case started to its end.
var finishRewriteStep = goldenStep{directive: "finish-rewrite", do: func(r *commandGoldenRun) {
	for i := 0; r.e.RewriteActive(); i++ {
		r.flush()
		require.Less(r.t, i, 200000, "the rewrite did not end")
		time.Sleep(20 * time.Microsecond)
	}
}}

// restoreOf sends KEEL.RESTORE dst with the image KEEL.DUMP gives for src,
// so that a case can restore a key without holding its image.
func restoreOf(name, dst, src string) goldenStep {
	return goldenStep{args: []string{name, dst, "<dump of " + src + ">"}, do: func(r *commandGoldenRun) {
		var w replyWriter
		require.NoError(r.t, r.e.EvalAndResponse(&Command{Cmd: "KEEL.DUMP", Args: []string{src}}, &w))
		image, err := Decode(w.b)
		require.NoError(r.t, err)
		r.pending = []string{name, dst, image.(string)}
	}}
}

// commandGoldenRun is one case running in one protocol.
type commandGoldenRun struct {
	t     *testing.T
	e     *Engine
	resp3 bool
	// path is the log's, and offset how much of it the steps so far
	// appended.
	path   string
	offset int64
	tx     *Transaction
	start  int64
	// pending is a command a directive prepared to be sent in its place.
	pending []string
	steps   []golden.Step
	records [][][]string
}

// startCommandGoldenRun makes the engine a case runs on, and opens its log.
func startCommandGoldenRun(t *testing.T, cs commandGoldenCase, resp3 bool) *commandGoldenRun {
	t.Helper()
	o := Options{Fsync: FsyncNever, AutoRewritePercentage: Off}
	if cs.options != nil {
		cs.options(&o)
	}
	r := &commandGoldenRun{t: t, e: newTestEngine(t, o), resp3: resp3}
	if !cs.noLog {
		r.path = filepath.Join(t.TempDir(), "golden.aof")
		require.NoError(t, r.e.OpenAOF(r.path))
	}
	r.start = time.Now().UnixMilli()
	return r
}

// flush writes the log as the event loop does after a cycle. While the log
// is retrying a failed write, the flush's error is the retry's, as the loop
// treats it.
func (r *commandGoldenRun) flush() {
	r.t.Helper()
	if r.path == "" {
		return
	}
	if err := r.e.FlushAOF(); err != nil && !r.e.LogRetrying() {
		require.NoError(r.t, err)
	}
}

// appended reads what the steps so far added to the log, as records.
func (r *commandGoldenRun) appended() [][]string {
	r.t.Helper()
	if r.path == "" {
		return nil
	}
	f, err := os.Open(r.path)
	require.NoError(r.t, err)
	defer f.Close()
	info, err := f.Stat()
	require.NoError(r.t, err)
	if info.Size() < r.offset {
		// A rewrite replaced the file; the case says its records are not
		// compared.
		r.offset = info.Size()
		return nil
	}
	body := make([]byte, info.Size()-r.offset)
	_, err = f.ReadAt(body, r.offset)
	require.NoError(r.t, err)
	r.offset = info.Size()
	if len(body) == 0 {
		return nil
	}
	return goldenRecords(r.t, body)
}

// step runs one step of the case and records what it did.
func (r *commandGoldenRun) step(s goldenStep) {
	r.t.Helper()
	if s.directive != "" {
		s.do(r)
		r.steps = append(r.steps, golden.Step{Directive: s.directive})
		r.records = append(r.records, r.appended())
		return
	}
	args := s.args
	if s.do != nil {
		s.do(r)
		args, r.pending = r.pending, nil
	}
	cmd := parsedCommand(args[0], args[1:])
	cmd.RESP3 = r.resp3
	var w replyWriter
	var err error
	if r.tx != nil || IsTransactionCommand(cmd.Cmd) {
		r.tx, err = r.e.Transact(r.tx, cmd, &w, nil)
	} else {
		err = r.e.EvalAndResponse(cmd, &w)
	}
	reply := w.b
	if err != nil {
		// What the server answers for the one thing a command cannot
		// answer for itself: being unknown (responseRw).
		reply = Encode(err, false)
	}
	r.flush()
	normalized, nerr := golden.Normalize(reply, s.flags)
	require.NoError(r.t, nerr, "%v answered %q", args, reply)
	step := golden.Step{Args: slices.Clone(s.args), Flags: slices.Clone(s.flags)}
	if s.do != nil {
		// The command a directive prepared is held as it was sent.
		step.Args = args
	}
	step.Replies[0], step.Replies[1] = golden.Held(string(normalized)), golden.Held(string(normalized))
	r.steps = append(r.steps, step)
	r.records = append(r.records, golden.NormalizeRecords(r.appended(), s.flags))
}

// finish ends the run and returns its case as the fixture holds it, with
// the records normalized against the run's window and the long strings held
// as digests.
func (r *commandGoldenRun) finish(cs commandGoldenCase) golden.Case {
	r.t.Helper()
	require.Nil(r.t, r.tx, "a transaction was left open")
	window := goldenWindow{r.start, time.Now().UnixMilli()}
	out := golden.Case{Name: cs.fullName()}
	for i, s := range r.steps {
		for j := range s.Args {
			if j > 0 {
				s.Args[j] = golden.Held(s.Args[j])
			}
		}
		if !cs.noRecords {
			// A directive's records are its own: an expiry cycle's DELs.
			for _, parts := range r.records[i] {
				parts = slices.Clone(parts)
				if strings.EqualFold(parts[0], "PEXPIREAT") && len(parts) == 3 {
					at, err := strconv.ParseUint(parts[2], 10, 64)
					require.NoError(r.t, err)
					parts[2] = strconv.FormatUint(window.instant(at), 10)
				}
				for j := range parts {
					parts[j] = golden.Held(parts[j])
				}
				s.Records = append(s.Records, parts)
			}
		}
		out.Steps = append(out.Steps, s)
	}
	return out
}

// runCommandGoldenCase runs cs in both protocols and returns its case: the
// RESP2 run's replies beside the RESP3 run's, and the records both wrote.
func runCommandGoldenCase(t *testing.T, cs commandGoldenCase) golden.Case {
	t.Helper()
	var runs [2]golden.Case
	for i, resp3 := range []bool{false, true} {
		r := startCommandGoldenRun(t, cs, resp3)
		for _, s := range cs.steps {
			r.step(s)
		}
		runs[i] = r.finish(cs)
	}
	require.Equal(t, len(runs[0].Steps), len(runs[1].Steps))
	out := runs[0]
	for i := range out.Steps {
		out.Steps[i].Replies[1] = runs[1].Steps[i].Replies[1]
		require.Equal(t, out.Steps[i].Records, runs[1].Steps[i].Records,
			"%s: step %d %v appended different records on a RESP3 connection", cs.fullName(), i+1, out.Steps[i].Args)
	}
	return out
}

// goldenFamilies are the corpus's families in the order they first appear,
// each with its cases.
func goldenFamilies() ([]string, map[string][]commandGoldenCase) {
	var order []string
	byFamily := map[string][]commandGoldenCase{}
	for _, cs := range commandGoldenCorpus {
		if _, seen := byFamily[cs.family]; !seen {
			order = append(order, cs.family)
		}
		byFamily[cs.family] = append(byFamily[cs.family], cs)
	}
	return order, byFamily
}

// commandGoldenManifest is the fixture's manifest.
type commandGoldenManifest struct {
	SourceRevision string                                `json:"source_revision"`
	Captured       string                                `json:"captured"`
	Note           string                                `json:"note"`
	Files          map[string]commandGoldenManifestEntry `json:"files"`
}

type commandGoldenManifestEntry struct {
	SHA256   string `json:"sha256"`
	Cases    int    `json:"cases"`
	Commands int    `json:"commands"`
	Records  int    `json:"records"`
}

func commandGoldenEntry(body []byte, cases []golden.Case) commandGoldenManifestEntry {
	sum := sha256.Sum256(body)
	entry := commandGoldenManifestEntry{SHA256: hex.EncodeToString(sum[:]), Cases: len(cases)}
	for _, c := range cases {
		for _, s := range c.Steps {
			if s.Directive == "" {
				entry.Commands++
				entry.Records += len(s.Records)
			}
		}
	}
	return entry
}

// explainGoldenDifference names the first step at which got differs from
// want.
func explainGoldenDifference(t *testing.T, want, got golden.Case) {
	t.Helper()
	for i := range min(len(want.Steps), len(got.Steps)) {
		w, g := want.Steps[i], got.Steps[i]
		at := fmt.Sprintf("%s: step %d of %d", want.Name, i+1, len(want.Steps))
		switch {
		case w.Directive != g.Directive:
			t.Errorf("%s: directive %q, want %q", at, g.Directive, w.Directive)
		case !slices.Equal(w.Args, g.Args):
			t.Errorf("%s: command %q, want %q", at, g.Args, w.Args)
		case !slices.Equal(w.Flags, g.Flags):
			t.Errorf("%s %q: normalizers %v, want %v", at, g.Args, g.Flags, w.Flags)
		case w.Replies[0] != g.Replies[0]:
			t.Errorf("%s %q: RESP2 reply\n got %q\nwant %q", at, g.Args, g.Replies[0], w.Replies[0])
		case w.Replies[1] != g.Replies[1]:
			t.Errorf("%s %q: RESP3 reply\n got %q\nwant %q", at, g.Args, g.Replies[1], w.Replies[1])
		case !slices.EqualFunc(w.Records, g.Records, slices.Equal):
			t.Errorf("%s %q: log records\n got %q\nwant %q", at, g.Args, g.Records, w.Records)
		default:
			continue
		}
		return
	}
	if len(want.Steps) != len(got.Steps) {
		t.Errorf("%s: %d steps, want %d", want.Name, len(got.Steps), len(want.Steps))
	}
}

// TestCommandGoldenCapture writes the fixture when asked to, and is skipped
// otherwise. It is run through scripts/capture-command-goldens.py. Every
// case runs three times in each protocol first, and a case whose replies or
// records differ between runs is refused, with the step named: it needs a
// normalizer, or different arguments.
func TestCommandGoldenCapture(t *testing.T) {
	t.Parallel()
	dir := os.Getenv("KEEL_CAPTURE_COMMAND_GOLDEN")
	if dir == "" {
		t.Skip("set KEEL_CAPTURE_COMMAND_GOLDEN to write the fixture")
	}
	dir = filepath.Join(dir, "core")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	manifest := commandGoldenManifest{
		SourceRevision: commandGoldenRevision,
		Captured:       time.Now().UTC().Format("2006-01-02"),
		Note:           "Every command's RESP2 and RESP3 replies and log records on develop, before phase 4 of the embedding plan; see command_golden_test.go.",
		Files:          map[string]commandGoldenManifestEntry{},
	}
	order, byFamily := goldenFamilies()
	for _, family := range order {
		var cases []golden.Case
		for _, cs := range byFamily[family] {
			first := runCommandGoldenCase(t, cs)
			for i := 0; i < 2; i++ {
				again := runCommandGoldenCase(t, cs)
				if !slices.EqualFunc(first.Steps, again.Steps, goldenStepsEqual) {
					explainGoldenDifference(t, first, again)
					t.Fatalf("%s is not the same from run to run; normalize the step named above", cs.fullName())
				}
			}
			cases = append(cases, first)
		}
		var body strings.Builder
		require.NoError(t, golden.Write(&body, cases))
		name := family + ".txt"
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body.String()), 0o644))
		manifest.Files[name] = commandGoldenEntry([]byte(body.String()), cases)
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), append(body, '\n'), 0o644))
}

func goldenStepsEqual(a, b golden.Step) bool {
	return a.Directive == b.Directive && slices.Equal(a.Args, b.Args) && slices.Equal(a.Flags, b.Flags) &&
		a.Replies == b.Replies && slices.EqualFunc(a.Records, b.Records, slices.Equal)
}

// loadCommandGoldens reads the fixture: its manifest, checked against each
// file's bytes, and every case by name.
func loadCommandGoldens(t *testing.T) (commandGoldenManifest, map[string]golden.Case) {
	t.Helper()
	var manifest commandGoldenManifest
	body, err := os.ReadFile(filepath.Join(commandGoldenDir, "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &manifest))
	require.Equal(t, commandGoldenRevision, manifest.SourceRevision, "the fixture's revision")
	cases := map[string]golden.Case{}
	for name, entry := range manifest.Files {
		body, err := os.ReadFile(filepath.Join(commandGoldenDir, name))
		require.NoError(t, err)
		sum := sha256.Sum256(body)
		require.Equal(t, entry.SHA256, hex.EncodeToString(sum[:]), "%s: the fixture's bytes", name)
		parsed, err := golden.Parse(strings.NewReader(string(body)))
		require.NoError(t, err, name)
		require.Equal(t, entry, commandGoldenEntry(body, parsed), "%s: the manifest's counts", name)
		for _, c := range parsed {
			_, dup := cases[c.Name]
			require.False(t, dup, "%s holds %s twice", name, c.Name)
			cases[c.Name] = c
		}
	}
	return manifest, cases
}

// TestCommandGoldenRepliesAreUnchanged: every case of the corpus answers, in
// both protocols, and records, what develop did at the fixture's revision.
func TestCommandGoldenRepliesAreUnchanged(t *testing.T) {
	t.Parallel()
	if os.Getenv("KEEL_CAPTURE_COMMAND_GOLDEN") != "" {
		t.Skip("capturing")
	}
	_, fixture := loadCommandGoldens(t)
	named := map[string]bool{}
	for _, cs := range commandGoldenCorpus {
		require.False(t, named[cs.fullName()], "the corpus holds %s twice", cs.fullName())
		named[cs.fullName()] = true
		want, ok := fixture[cs.fullName()]
		require.True(t, ok, "%s is in the corpus and not in the fixture: capture it", cs.fullName())
		t.Run(cs.fullName(), func(t *testing.T) {
			t.Parallel()
			got := runCommandGoldenCase(t, cs)
			explainGoldenDifference(t, want, got)
		})
	}
	for name := range fixture {
		require.True(t, named[name], "%s is in the fixture and not in the corpus", name)
	}
}
