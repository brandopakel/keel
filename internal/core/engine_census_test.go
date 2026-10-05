package core

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Why a package variable may stay, for now or for good.
//
// The embedding plan (docs/embedding-plan.md) moves this package's mutable
// state into Engine, a step at a time, so that two engines in one process share
// nothing. A new package variable would quietly undo that, and nothing would
// notice until two instances disagreed. So the census below parses the
// package's source and fails on any top-level variable packageVars does not
// list. Each mutable entry names the step that takes it into the engine, and
// that step removes it here; an entry whose variable is gone fails the census
// too, so the list cannot outlive what it excuses.
const (
	// For good: values that are computed once and never written again.
	sentinel = "an error sentinel, matched by identity and never reassigned"
	table    = "a table or reply built once and only read after"

	// Until the step that moves it.
	defaultInstance = "the server's engine until callers open their own (plan step 2.7)"
	replicated      = "replication or failover state, or its I/O; becomes engine state (plan step 2.4)"
	transport       = "a hook the server installs for INFO; goes when the server renders INFO (plan phase 6)"
)

// packageVars is every package-level variable this package may declare, with
// the reason each one is allowed.
var packageVars = map[string]string{
	"defaultEngine": defaultInstance,

	"failover": replicated, "termRename": replicated, "termSync": replicated, "termSyncDir": replicated,

	"ClientBuffers": transport,

	"commandTable": table, "commandArity": table, "commands": table,
	"connectionCommands": table, "containerCommands": table, "clientSubcommands": table,
	"memorySubcommands": table, "commandKeyspace": table, "multiKeyCommands": table,
	"strideKeyCommands": table, "argumentsBeforeType": table, "replacingWrites": table,
	"filterCommands": table, "writeCommands": table, "notInTransaction": table,
	"answersWithoutData": table, "bfInfoOptions": table, "cfOptions": table, "memoryHelp": table,
	"crlf": table, "queuedReply": table, "execAbortReply": table, "allocationPressure": table,
	"replyTooLarge": table, "transactionOpenFrame": table, "transactionCloseFrame": table,

	"errTruncatedAOF": sentinel, "errTooLargeForBudget": sentinel, "errTooLargeForOneKey": sentinel,
	"errRewriteCantStart": sentinel, "errRewriteInProgress": sentinel,
	"errBFBadCapacity": sentinel, "errBFBadErrorRate": sentinel, "errBFBadExpansion": sentinel,
	"errBFCapacityRange": sentinel, "errBFCouldNotCreate": sentinel, "errBFErrorRateRange": sentinel,
	"errBFExpansionRange": sentinel, "errBFInvalidInfoName": sentinel, "errBFNoExpansion": sentinel,
	"errBFNonScalingGrow": sentinel, "errCFBadCapacity": sentinel, "errCFCapacityRange": sentinel,
	"errCFDelNotFound": sentinel, "errCFFull": sentinel, "errCFGeometry": sentinel,
	"errCMSDepth": sentinel, "errCMSErrRate": sentinel, "errCMSExists": sentinel, "errCMSMissing": sentinel,
	"errCMSNumber": sentinel, "errCMSOverflow": sentinel, "errCMSProb": sentinel, "errCMSWidth": sentinel,
	"errDecrOverflow": sentinel, "errIncrOverflow": sentinel, "errGeoUnit": sentinel,
	"errIntegerOutOfRange": sentinel, "errRandomCountRange": sentinel, "errNotAFloat": sentinel,
	"errNXWithXX": sentinel, "errSyntax": sentinel, "errMinMaxNotFloat": sentinel,
	"errFenced": sentinel, "errLCSType": sentinel, "errNotHLL": sentinel, "errWrongType": sentinel,
	"errCountNegative": sentinel, "errNotAnInteger": sentinel,
	"errFilterExists": sentinel, "errFilterNotFound": sentinel, "errReadOnlyReplica": sentinel,
	"ErrRequestAllocation": sentinel, "ErrIncompleteFrame": sentinel, "ErrProtocol": sentinel,
	"errDiscardNoMulti": sentinel, "errExecNoMulti": sentinel, "errNestedMulti": sentinel,
	"errNoReply": sentinel, "errNotInTransaction": sentinel, "ErrTransactionReplyTooLarge": sentinel,
	"errTransactionTooLarge": sentinel, "errWatchInMulti": sentinel,
}

// TestPackageStateIsCensused reads the package's own declarations rather than
// trusting review to spot a new global. Test files are left out: what they
// declare is not in the package a caller links.
func TestPackageStateIsCensused(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	declared := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				for _, id := range spec.(*ast.ValueSpec).Names {
					if id.Name == "_" {
						continue // a reference kept for the compiler holds nothing
					}
					declared[id.Name] = true
					if _, ok := packageVars[id.Name]; !ok {
						t.Errorf("%s: package variable %s would be shared by every Engine in the process; "+
							"make it an Engine field, or list it in packageVars with the reason it may stay",
							fset.Position(id.Pos()), id.Name)
					}
				}
			}
		}
	}
	require.NotEmpty(t, declared, "the census found no source to read")
	for name := range packageVars {
		if !declared[name] {
			t.Errorf("packageVars lists %s, which is no longer declared; remove the entry", name)
		}
	}
}
