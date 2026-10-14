package core

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/brandopakel/keel/internal/golden"
)

// What the command corpus has to cover, read against the package's own
// tables and source rather than left to review: every command, with a
// record for every write, and every error text a handler can answer with.
// A command or an error added to the package without a case in the corpus
// fails here, so the corpus cannot fall behind what it gates.

// goldenCorpusCommands is every command the corpus sends, upper-cased, with
// whether any step of it appended a record.
func goldenCorpusCommands(t *testing.T) map[string]bool {
	t.Helper()
	_, fixture := loadCommandGoldens(t)
	recorded := map[string]bool{}
	for _, c := range fixture {
		for _, s := range c.Steps {
			if s.Directive != "" {
				continue
			}
			name := strings.ToUpper(s.Args[0])
			recorded[name] = recorded[name] || len(s.Records) > 0
			if subcommandsOf(name) != nil && len(s.Args) > 1 {
				sub := name + "|" + strings.ToUpper(s.Args[1])
				recorded[sub] = recorded[sub] || len(s.Records) > 0
			}
		}
	}
	return recorded
}

// TestCommandGoldensCoverEveryCommand: every command the engine runs is sent
// by the corpus, and every command the log takes is sent in a form the log
// recorded, so the fixture holds a transcript of each write.
func TestCommandGoldensCoverEveryCommand(t *testing.T) {
	t.Parallel()
	if os.Getenv("KEEL_CAPTURE_COMMAND_GOLDEN") != "" {
		t.Skip("capturing")
	}
	recorded := goldenCorpusCommands(t)
	for name := range commandArity {
		if connectionCommands[name] {
			continue // the server's corpus, internal/server/connection_golden_test.go
		}
		_, sent := recorded[name]
		require.True(t, sent, "%s is not sent by the command corpus", name)
	}
	for name := range writeCommands {
		require.True(t, recorded[name], "%s is a write command and no step of the corpus has it recorded", name)
	}
	// Every subcommand of a container the engine answers: MEMORY's and
	// CONFIG's. CLIENT's are the connection corpus's.
	for name := range commandArity {
		if connectionCommands[name] {
			continue
		}
		for _, sub := range subcommandsOf(name) {
			full := name + "|" + strings.ToUpper(sub.name)
			_, sent := recorded[full]
			require.True(t, sent, "%s is not sent by the command corpus", full)
		}
	}
}

// uncoveredErrorTexts are the error texts the handlers hold that no case of
// the corpus can reach from a command, each with the reason. A text that a
// case does reach has to leave this list, and a text that leaves the source
// has to leave it too.
var uncoveredErrorTexts = map[string]string{
	"ERR Invalid arguments specified for command":                                "COMMAND GETKEYS of a command whose key spec cannot be followed, a keyword or keynum spec, which no command Keel has has",
	"ERR command produced no reply":                                              "EXEC's stand-in for a command that wrote nothing, which no command does",
	"ERR transaction exceeds the %d MiB queued command limit":                    "queueing 16 MiB of commands, which the corpus does not hold",
	"ERR persisting term: %w":                                                    "a term file that cannot be written: a disk failure",
	"ERR recording term: %w":                                                     "a term file that cannot be written: a disk failure",
	"ERR preparing replication snapshot: %w":                                     "a snapshot the log cannot start: a disk failure",
	"ERR replication history gap":                                                "a protocol 2 pull from history the primary no longer holds, which needs a stream of its own",
	"ERR replication snapshot exceeds 1 GiB":                                     "a snapshot over 1 GiB, which the corpus does not build",
	"ERR replication snapshot exceeds 8 MiB":                                     "a protocol 1 snapshot over 8 MiB, which the corpus does not build",
	"ERR replication snapshot limit":                                             "a protocol 1 snapshot of over 100,000 keys, which the corpus does not build",
	"ERR invalid snapshot position":                                              "a protocol 2 pull into a snapshot that is not there, which needs a stream of its own",
	"ERR SCAN pattern work limit exceeded; use a simpler MATCH or smaller COUNT": "a MATCH that costs a million steps of backtracking against the keys of one SCAN",
	"ERR Can't execute an AOF background rewriting. Please check the server logs for more information.": "a rewrite whose file cannot be opened: a disk failure",
}

// TestCommandGoldensCoverEveryErrorText: every error text the package can
// answer with (golden.ErrorTexts) is answered somewhere in the fixture,
// apart from those listed above with their reason.
func TestCommandGoldensCoverEveryErrorText(t *testing.T) {
	t.Parallel()
	if os.Getenv("KEEL_CAPTURE_COMMAND_GOLDEN") != "" {
		t.Skip("capturing")
	}
	_, fixture := loadCommandGoldens(t)
	texts, err := golden.ErrorTexts(".")
	require.NoError(t, err)
	require.NotEmpty(t, texts)
	for _, problem := range golden.Uncovered(texts, golden.ReplyTexts(fixture), uncoveredErrorTexts) {
		t.Error(problem)
	}
}
