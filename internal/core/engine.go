package core

import "github.com/brandopakel/keel/internal/data_structure"

// Engine is one keyspace and the state that serves it.
//
// Everything this package keeps between commands used to be a package
// variable, which made the server a singleton: one keyspace per process, and no
// two tests able to run side by side. The embedding plan
// (docs/embedding-plan.md) gives all of it an owner, so that a process can hold
// several independent instances and the server becomes one caller of the
// engine among others. Step 2.1 moves the stores, one command family at a
// time. A family that has moved keeps its store here, and its handlers are
// Engine methods that read the store from the engine dispatching them - see
// engineCommandTable. The families still to move read package variables, in
// storage.go, until they do.
//
// Until callers open engines of their own (plan phase 3) the server and the
// tests run on defaultEngine, as the stores run on data_structure.DefaultSpace.
//
// An Engine is not safe for concurrent use. Like the stores in it, it belongs
// to whoever is executing commands: the event loop's thread today, and the
// holder of the engine's lock once there is one (plan phase 3).
type Engine struct {
	// space is what the engine's stores have in common: the registry eviction
	// draws from, its clock and its limits.
	space *data_structure.Space

	// The stores that have moved, under the names their package variables
	// had, so that what a family reads is found by the same search as before.
	hashStore *data_structure.Keyed[*data_structure.Hash]
}

// defaultEngine is the engine the server and the tests run on until each
// caller opens its own; plan step 2.7 removes it. It lives in DefaultSpace, so
// its limits are read from config, as the server's always have been.
//
// The pointer never changes. ResetStores rebuilds the stores inside it, so
// whatever has kept the engine keeps the keyspace a test began from empty.
var defaultEngine = &Engine{space: data_structure.DefaultSpace}
