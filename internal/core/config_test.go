package core

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// configPairs runs CONFIG GET and reads its flat RESP2 reply into name and
// value pairs, in order.
func configPairs(t testing.TB, e *Engine, patterns ...string) [][2]string {
	t.Helper()
	reply, ok := runOn(t, e, "CONFIG", append([]string{"GET"}, patterns...)...).([]interface{})
	require.True(t, ok, "CONFIG GET answers an array")
	require.Zero(t, len(reply)%2)
	var pairs [][2]string
	for i := 0; i < len(reply); i += 2 {
		pairs = append(pairs, [2]string{reply[i].(string), reply[i+1].(string)})
	}
	return pairs
}

// TestConfigGetReportsKeelsSettingsUnderRedisNames: the settings Keel has,
// each under Redis's name and in Redis's format, read live from the engine.
func TestConfigGetReportsKeelsSettingsUnderRedisNames(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{MaxMemory: 1 << 20, Eviction: EvictLFU})
	for name, want := range map[string]string{
		"maxmemory": "1048576", "maxmemory-policy": "allkeys-lfu", "maxmemory-samples": "5",
		"lfu-log-factor": "10", "appendonly": "no", "appendfsync": "everysec",
		"auto-aof-rewrite-percentage": "100", "auto-aof-rewrite-min-size": "67108864",
		"databases": "1", "save": "", "replicaof": "", "slaveof": "",
	} {
		assert.Equal(t, [][2]string{{name, want}}, configPairs(t, e, name), name)
	}
	dir, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, [][2]string{{"dir", dir}}, configPairs(t, e, "dir"))

	reconfigure(t, e, func(o *Options) { o.MaxMemory = 2 << 20; o.Eviction = EvictRandom })
	assert.Equal(t, [][2]string{{"maxmemory", "2097152"}, {"maxmemory-policy", "allkeys-random"}},
		configPairs(t, e, "maxmemory", "maxmemory-policy"), "the live values")
}

// TestConfigGetMatchesAsRedisDoes: names without regard to case, spelled
// back as asked; glob patterns with Redis's spelling; each parameter once;
// nothing for a name Keel does not have.
func TestConfigGetMatchesAsRedisDoes(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, [][2]string{{"MAXMEMORY", "0"}}, configPairs(t, e, "MAXMEMORY"))
	assert.Equal(t, [][2]string{{"maxmemory", "0"}, {"maxmemory-policy", "allkeys-lru"}, {"maxmemory-samples", "5"}},
		configPairs(t, e, "MAXMEM*"))
	assert.Equal(t, [][2]string{{"appendonly", "no"}, {"maxmemory", "0"}},
		configPairs(t, e, "appendonly", "maxmemory", "maxmemory", "append?nly", "nosuch"))
	assert.Empty(t, configPairs(t, e, "nosuch", "lfu-decay-time"), "a name Keel has no counterpart for")
	assert.Equal(t, [][2]string{{"replicaof", ""}, {"slaveof", ""}}, configPairs(t, e, "[rs]*of"))
}

// TestConfigGetReportsTheTransportsSettingsOnceItHasSaid: port, bind and the
// rest come from the transport, through SetServerInfo, and are absent without.
func TestConfigGetReportsTheTransportsSettingsOnceItHasSaid(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Empty(t, configPairs(t, e, "port", "bind", "maxclients", "hz", "requirepass"))
	e.SetServerInfo(&ServerInfo{Port: 6390, MaxClients: 50, IOThreads: 4, Hz: 10, Multiplexer: "epoll",
		Host: "0.0.0.0", RequirePass: "secret"})
	got := map[string]string{}
	for _, p := range configPairs(t, e, "*") {
		got[p[0]] = p[1]
	}
	for name, want := range map[string]string{"port": "6390", "bind": "0.0.0.0", "maxclients": "50",
		"tcp-backlog": "50", "io-threads": "4", "hz": "10", "requirepass": "secret"} {
		assert.Equal(t, want, got[name], name)
	}
}

// TestConfigReplicaofIsWrittenAsRedisWritesIt: host and port separated by a
// space, as Redis writes a primary.
func TestConfigReplicaofIsWrittenAsRedisWritesIt(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	e.role.ReplicaOf = "10.0.0.1:6380"
	for _, p := range e.configParameters() {
		if p.name == "replicaof" || p.name == "slaveof" {
			assert.Equal(t, "10.0.0.1 6380", p.value, p.name)
		}
	}
}

// TestConfigRepliesInEitherProtocol: a flat array in RESP2 and a map in RESP3,
// as Redis answers; HELP and the refusals in Redis's words.
func TestConfigRepliesInEitherProtocol(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	assert.Equal(t, "*2\r\n$9\r\nmaxmemory\r\n$1\r\n0\r\n", string(rawReplyAsOn(t, e, false, "CONFIG", "GET", "maxmemory")))
	assert.Equal(t, "%1\r\n$9\r\nmaxmemory\r\n$1\r\n0\r\n", string(rawReplyAsOn(t, e, true, "CONFIG", "GET", "maxmemory")))
	assert.Equal(t, "*0\r\n", string(rawReplyOn(t, e, "CONFIG", "GET", "nosuch")))

	assert.Equal(t, []interface{}{"CONFIG <subcommand> [<arg> [value] [opt] ...]. Subcommands are:",
		"GET <pattern>", "    Return parameters matching the glob-like <pattern> and their values.",
		"SET <directive> <value>", "    Set the configuration <directive> to <value>.",
		"RESETSTAT", "    Reset statistics reported by the INFO command.",
		"REWRITE", "    Rewrite the configuration file.",
		"HELP", "    Print this help."}, runOn(t, e, "CONFIG", "HELP"))
	for args, want := range map[string]string{
		"":              "-ERR wrong number of arguments for 'config' command\r\n",
		"GET":           "-ERR wrong number of arguments for 'config|get' command\r\n",
		"SET a":         "-ERR wrong number of arguments for 'config|set' command\r\n",
		"resetstat x":   "-ERR wrong number of arguments for 'config|resetstat' command\r\n",
		"nosuch":        "-ERR unknown subcommand 'nosuch'. Try CONFIG HELP.\r\n",
		"HELP extra":    "-ERR wrong number of arguments for 'config|help' command\r\n",
		"REWRITE":       "-ERR The server is running without a config file\r\n",
		"rewrite extra": "-ERR wrong number of arguments for 'config|rewrite' command\r\n",
	} {
		assert.Equal(t, want, string(rawReplyOn(t, e, "CONFIG", strings.Fields(args)...)), args)
	}
}

// TestConfigSetRefusesAsRedisRefusesASettingItCannotSet: until Keel has
// runtime configuration, every pair is refused with Redis's error for the
// first that fails: a name it does not know, or one it cannot change.
func TestConfigSetRefusesAsRedisRefusesASettingItCannotSet(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for args, want := range map[string]string{
		"maxmemory 1":                 "-ERR CONFIG SET failed (possibly related to argument 'maxmemory') - can't set immutable config\r\n",
		"MaxMemory-Policy noeviction": "-ERR CONFIG SET failed (possibly related to argument 'MaxMemory-Policy') - can't set immutable config\r\n",
		"slaveof x":                   "-ERR CONFIG SET failed (possibly related to argument 'slaveof') - can't set immutable config\r\n",
		"dir /tmp":                    "-ERR CONFIG SET failed (possibly related to argument 'dir') - can't set protected config\r\n",
		"notify-keyspace-events Ex":   "-ERR Unknown option or number of arguments for CONFIG SET - 'notify-keyspace-events'\r\n",
		"nosuch 1 maxmemory 1":        "-ERR Unknown option or number of arguments for CONFIG SET - 'nosuch'\r\n",
		"maxmemory 1 nosuch 1":        "-ERR CONFIG SET failed (possibly related to argument 'maxmemory') - can't set immutable config\r\n",
		"maxmemory 1 hz":              "-ERR syntax error\r\n",
	} {
		assert.Equal(t, want, string(rawReplyOn(t, e, "CONFIG", append([]string{"SET"}, strings.Fields(args)...)...)), args)
	}
	assert.Equal(t, [][2]string{{"maxmemory", "0"}}, configPairs(t, e, "maxmemory"), "nothing was set")
}
