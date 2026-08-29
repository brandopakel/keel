package data_structure

type Set interface {
	Add(members ...string) int
	Rem(members ...string) int
	Size() int
	IsMember(member string) int
	MIsMember(members ...string) []int
	Members() []string
	Pop(count int) []string
	Rand(count int) []string
	// MemUsage estimates the bytes held, so a set can be weighed against every
	// other kind of key under a memory budget.
	MemUsage() uint64
}

type MultiSetOperator interface {
	Move(src, dest Set, members ...string) int
	Inter(keys ...Set) []string
	InterCard(keys ...Set) int
	InterStore(keys ...Set) Set
	Diff(keys ...Set) []string
	DiffStore(keys ...Set) Set
	Union(keys ...Set) []string
	UnionStore(keys ...Set) Set
}

func CreateSet(key string) Set {
	return newSimpleSet(key)
}
