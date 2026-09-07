package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/brandopakel/keel/internal/config"
)

// A term names a period of leadership. It is a stale-generation guard, and it is
// explicitly not a fence.
//
// The distinction matters and docs/failover-design.md gives the counterexample:
// if a primary at term 5 is partitioned from the coordinator but still reachable
// by clients, promoting another node at term 6 does not teach it about term 6.
// Both accept writes. Demoting on a higher term only protects a node that
// receives the message, and a partitioned node receives nothing.
//
// So nothing here establishes "at most one writer". What it does establish is
// narrower and still worth having: a node that has *learned* of a higher term
// stops writing at once, obsolete replication histories are rejected rather than
// followed, and a promotion cannot be claimed with a term the node has already
// seen. Exclusivity has to come from an external fencing authority that can
// isolate the old incarnation's data paths - see the design document.
//
// The term is deliberately not the replication epoch. The epoch identifies a
// primary's history so a replica can tell whether its offsets still mean
// anything, and it changes on restart. The term identifies authority and rises
// only on promotion. One value cannot do both, because a primary restarting has
// to invalidate offsets without claiming new authority.
type failoverState struct {
	// term is the highest term this node has seen. Successful observations are
	// persisted; a storage failure still revokes live write authority. Atomic
	// access publishes it to the replication transport's reader.
	term uint64
	// persisted is the last successfully synced observation. Failed updates
	// remain pending, so retrying the same term cannot falsely acknowledge it.
	persisted uint64

	// fenced is set when this node has seen a term above the one it holds. It
	// refuses writes from that moment, without waiting to be told twice and
	// without finishing what it was doing.
	fenced bool

	// held is the term this node was promoted at. A primary may write only
	// while held equals term.
	held uint64

	path string
}

var failover failoverState

// termFileName is beside the log, because a term is only meaningful for the
// data it authorises writes to.
const termFileName = ".term"

var termRename = os.Rename
var termSyncDir = syncDir
var termSync = func(f *os.File) error { return f.Sync() }

// LoadTerm reads the durable term. Call once, before serving.
//
// A missing file is term zero: a node that has never been promoted, which is
// every node before its first failover and every fresh install. A file that
// exists but cannot be read or parsed is different in kind - something was
// there and is now damaged - and a primary refuses to start on it rather than
// guess a term the cluster may already have moved past. Replicas also refuse
// damaged state: being read-only does not make following a stale history safe.
//
// The limit this cannot see is a storage rollback that removes the file
// entirely, which is indistinguishable from a node that was never promoted.
// Nothing local can tell those apart; it is why the coordinator, not the node,
// has to be the one that never issues a term twice.
func LoadTerm(path string) error {
	failover.path = path + termFileName
	atomic.StoreUint64(&failover.term, 0)
	failover.persisted = 0
	failover.held, failover.fenced = 0, false

	body, err := os.ReadFile(failover.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("unreadable term file %s: %w", failover.path, err)
	}
	term, parseErr := strconv.ParseUint(strings.TrimSpace(string(body)), 10, 64)
	if parseErr != nil {
		return fmt.Errorf("damaged term file %s: %w", failover.path, parseErr)
	}
	atomic.StoreUint64(&failover.term, term)
	failover.persisted = term
	// The numeric file records observation, not a durable grant to this
	// incarnation. Every nonzero-term primary starts fenced and needs a fresh
	// externally assigned higher term. Restart must not claim a successor's term.
	failover.fenced = config.ReplicaOf == "" && term > 0
	return nil
}

// persistTerm makes a term durable before granting local write authority.
// Observing a successor revokes authority even if storage is failing; only a
// successful persistence operation can promise recovery of that observation.
func persistTerm(term uint64) error {
	if failover.path == "" {
		return errors.New("no term file: failover requires an append-only log")
	}
	dir := filepath.Dir(failover.path)
	f, err := os.CreateTemp(dir, ".keel-term-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.WriteString(strconv.FormatUint(term, 10)); err == nil {
		err = termSync(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = termRename(tmp, failover.path); err != nil {
		return err
	}
	if err = termSyncDir(dir); err != nil {
		return err
	}
	failover.persisted = term
	return nil
}

// observeTerm records a term learned from another node.
//
// A term above this node's deposes it at once, which covers the case where the
// old primary can still talk to the cluster - it stands down on the first
// exchange rather than finishing what it was doing. It does nothing for a node
// that cannot hear anyone, which is the case a fence exists for. The term is
// persisted before success is reported. A persistence failure still stops live
// writes and requires external isolation/recovery rather than continuing to write.
func observeTerm(term uint64) error {
	if term > failover.term {
		atomic.StoreUint64(&failover.term, term)
		// Only a primary needs fencing. Replicas already refuse writes.
		if config.ReplicaOf == "" && failover.held < term {
			failover.fenced = true
		}
	}
	if failover.persisted < failover.term {
		return persistTerm(failover.term)
	}
	return nil
}

// Writable reports whether this node may accept a write.
func Writable() bool {
	return config.ReplicaOf == "" && !failover.fenced && failover.held == failover.term
}

// CurrentTerm and HeldTerm are what INFO reports.
func CurrentTerm() uint64 { return atomic.LoadUint64(&failover.term) }
func HeldTerm() uint64    { return failover.held }
func Fenced() bool        { return failover.fenced }

var errFenced = errors.New("FENCED this node is not the holder of the current term")

// Writable is consulted per write rather than at a transition, so a node that
// has learned it was replaced stops immediately instead of at the next
// checkpoint. It cannot help a node that has learned nothing.

// cmdPROMOTE claims a term for this node.
//
// The term has to come from outside and has to be higher than any this node has
// seen: a node that could choose its own term could choose one that outranks a
// leader chosen properly. Equal is refused as well as lower, because a
// coordinator that issued the same term twice has already lost the property
// that makes a term worth anything.
func cmdPROMOTE(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'KEEL.PROMOTE' command"), false)
	}
	term, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return Encode(errNotAnInteger, false)
	}
	if term <= failover.term {
		return Encode(fmt.Errorf("ERR term %d is not above the current term %d", term, failover.term), false)
	}
	// On disk before a single write is taken at it.
	if err := observeTerm(term); err != nil {
		return Encode(fmt.Errorf("ERR persisting term: %w", err), false)
	}
	failover.held, failover.fenced = term, false
	return Encode("OK", true)
}

// cmdFENCE tells this node a term it does not hold now exists, which stands it
// down.
//
// The name describes what it does to this node, not a guarantee about the
// deployment. A coordinator calling it has told one node to stop; it has not
// established that the node stopped, and a node that is unreachable cannot be
// told. Real exclusivity needs an authority that can isolate the node's data
// paths whether or not the node cooperates.
func cmdFENCE(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'KEEL.FENCE' command"), false)
	}
	term, err := strconv.ParseUint(args[0], 10, 64)
	if err != nil {
		return Encode(errNotAnInteger, false)
	}
	if err := observeTerm(term); err != nil {
		return Encode(fmt.Errorf("ERR recording term: %w", err), false)
	}
	return Encode("OK", true)
}
