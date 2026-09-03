package core

import (
	"errors"

	"memkv/internal/constant"
	"memkv/internal/data_structure"
)

// Keyspace commands: the ones a client library reaches for before it reaches
// for anything interesting.
//
// go-redis calls TYPE to decide how to decode a value, EXISTS to avoid a round
// trip, and MGET wherever it can batch. Without them a client works only for as
// long as nobody uses its conveniences, which is a worse failure than not
// connecting at all - it fails later, and further from the cause.
//
// All of these answer across every keyspace rather than only strings, because a
// name here means one thing whatever type holds it. That is what makes EXISTS
// and TYPE answerable at all: OwnerOf already arbitrates between the stores.

func cmdEXISTS(args []string) []byte {
	if len(args) == 0 {
		return Encode(errors.New("ERR wrong number of arguments for 'EXISTS' command"), false)
	}

	// Repeats count repeatedly, which is Redis's behaviour from 3.0 on:
	// EXISTS k k is 2. It reads oddly and it is what clients expect.
	found := 0
	for _, key := range args {
		if _, held := data_structure.OwnerOf(key); held {
			found++
		}
	}
	return Encode(found, false)
}

// cmdTYPE reports which keyspace holds a name.
//
// For strings, sets and sorted sets the answer is the word Redis uses, so a
// client switching on it behaves the same here. The probabilistic types have no
// Redis equivalent to agree with - Redis keeps a Bloom filter in a module and a
// HyperLogLog in a plain string - so they answer with their own keyspace name
// rather than borrowing a word that would be a lie.
func cmdTYPE(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'TYPE' command"), false)
	}

	owner, held := data_structure.OwnerOf(args[0])
	if !held {
		// Redis answers +none rather than an error or a nil, and clients test
		// for exactly that string.
		return Encode("none", true)
	}
	return Encode(owner.KeyspaceName(), true)
}

// cmdKEYS lists every key matching a glob pattern.
//
// It walks the entire keyspace and builds the whole reply before sending any of
// it, which on a single-threaded server means every other client waits for it.
// That is Redis's KEYS too, and the reason Redis tells you to use SCAN instead.
// SCAN is not implemented here, and the note in the README says why rather than
// leaving it looking like an oversight.
func cmdKEYS(args []string) []byte {
	if len(args) != 1 {
		return Encode(errors.New("ERR wrong number of arguments for 'KEYS' command"), false)
	}
	pattern := args[0]

	var matches []string
	data_structure.EachKeyspace(func(ks data_structure.Keyspace) {
		for _, key := range ks.Keys() {
			// Pattern first: it is the cheap test, and Has can reap, which is
			// work worth doing only for a key that would otherwise be reported.
			if !globMatch(pattern, key) {
				continue
			}
			// Keys may include a key whose TTL has passed. Has settles it, and
			// reaps it on the way past, so KEYS never shows a key that GET
			// would say was gone.
			if ks.Has(key) {
				matches = append(matches, key)
			}
		}
	})

	if len(matches) == 0 {
		return constant.RespEmptyArray
	}
	return Encode(matches, false)
}

// cmdMGET reads several string keys at once.
//
// A key of another type reads as nil rather than WRONGTYPE, which is Redis's
// rule and the one place this server's stricter "one name, one thing" line is
// deliberately not drawn: MGET's whole purpose is to ask about many keys
// without the answer to one of them destroying the rest. That is also why it is
// absent from the type table in keytype.go.
func cmdMGET(args []string) []byte {
	if len(args) == 0 {
		return Encode(errors.New("ERR wrong number of arguments for 'MGET' command"), false)
	}

	out := make([]interface{}, 0, len(args))
	for _, key := range args {
		obj := dictStore.Get(key)
		if obj == nil {
			// nil encodes as a null bulk string, which is the element Redis
			// puts here for both a missing key and a key of the wrong type.
			out = append(out, nil)
			continue
		}
		out = append(out, obj.Value)
	}
	return Encode(out, false)
}

// cmdMSET writes several string keys at once.
//
// Unlike MGET this does type-check, because it writes. A name held by a set is
// refused rather than overwritten, which is this server's rule rather than
// Redis's - Redis lets SET clobber any type. Following SET here matters more
// than following Redis: MSET that behaved differently from SET on the same key
// would be the surprising one.
//
// Redis documents MSET as atomic, and on a server whose command execution is
// single-threaded it is, with nothing needed to make it so: no other client can
// observe a moment inside it. The type check having already run for every key
// is what makes that true of failures too - it cannot write half the pairs and
// then refuse.
func cmdMSET(args []string) []byte {
	if len(args) == 0 || len(args)%2 != 0 {
		return Encode(errors.New("ERR wrong number of arguments for 'MSET' command"), false)
	}

	for i := 0; i < len(args); i += 2 {
		key, value := args[i], args[i+1]
		oType, oEnc := deduceTypeString(value)
		dictStore.Put(key, dictStore.NewObj(value, oType, oEnc))
	}
	return constant.RespOk
}

// cmdFLUSHDB empties every keyspace.
//
// The keys are collected before any are deleted, because deleting while walking
// a store's own key slice is fine but marking the rewrite is not: a rewrite
// walking the keyspace has already written some of these keys into the new log,
// and unless it is told they changed it will finish by producing a log that
// restores everything FLUSHDB just removed. Marking them dirty makes
// finishRewrite emit a DEL for each and re-emit nothing, which is exactly right.
func cmdFLUSHDB(args []string) []byte {
	if len(args) != 0 {
		// ASYNC and SYNC are accepted by Redis and mean nothing on a server
		// that has no background free. Refusing is better than accepting a
		// word that promises something this does not do.
		return Encode(errors.New("ERR wrong number of arguments for 'FLUSHDB' command"), false)
	}

	data_structure.EachKeyspace(func(ks data_structure.Keyspace) {
		for _, key := range ks.Keys() {
			noteRewriteDirty(key)
			ks.Delete(key)
		}
	})
	return constant.RespOk
}
