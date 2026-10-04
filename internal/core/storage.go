package core

import (
	"github.com/brandopakel/keel/internal/data_structure"
)

// The keyspaces.
//
// Each type has its own store, and every one of them is registered with the
// evictor so that a memory budget spans the lot: before that, only strings were
// accounted, and a keyspace full of 12KB HyperLogLogs could sail past
// -maxmemory without anything noticing. The stores are fields of Engine; these
// are the server's, in defaultEngine.

func init() { ResetStores() }

// ResetStores rebuilds every keyspace and re-registers them. Called at startup,
// and by tests that need to begin from empty.
//
// The order they register in is the order OwnerOf asks them, eviction samples
// them and SCAN and the rewrite walk them, so it is the order they had as
// package variables.
func ResetStores() {
	e := defaultEngine
	space := e.space
	space.ResetKeyspaces()
	// The counter describes the keyspace being thrown away, so it goes with it.
	expiredKeys = 0
	aof.recovered = nil

	e.dictStore = data_structure.CreateDict(space)
	e.zsetStore = data_structure.NewKeyed[*data_structure.ZSet](space, "zset")
	e.setStore = data_structure.NewKeyed[*data_structure.Set](space, "set")
	e.hashStore = data_structure.NewKeyed[*data_structure.Hash](space, "hash")
	e.listStore = data_structure.NewKeyed[*data_structure.List](space, "list")
	e.sbStore = data_structure.NewKeyed[*data_structure.SBChain](space, "bloom")
	e.cmsStore = data_structure.NewKeyed[*data_structure.CMS](space, "cms")
	e.morrisStore = data_structure.NewKeyed[*data_structure.Morris](space, "morris")
	e.hllStore = data_structure.NewKeyed[*data_structure.HLL](space, "hll")
	e.cfStore = data_structure.NewKeyed[*data_structure.CuckooFilter](space, "cuckoo")

	space.RegisterKeyspace(e.dictStore)
	space.RegisterKeyspace(e.zsetStore)
	space.RegisterKeyspace(e.setStore)
	space.RegisterKeyspace(e.hashStore)
	space.RegisterKeyspace(e.listStore)
	space.RegisterKeyspace(e.sbStore)
	space.RegisterKeyspace(e.cmsStore)
	space.RegisterKeyspace(e.morrisStore)
	space.RegisterKeyspace(e.hllStore)
	space.RegisterKeyspace(e.cfStore)
}
