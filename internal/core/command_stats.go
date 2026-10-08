package core

// commandStat is one command's line of Redis's INFO commandstats.
type commandStat struct {
	calls, failed, rejected uint64
	nanos                   uint64
}
