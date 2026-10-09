package core

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/brandopakel/keel/internal/constant"
)

// configHelp is Redis's CONFIG HELP, for the subcommands this server has.
var configHelp = HelpReply("CONFIG",
	"GET <pattern>",
	"    Return parameters matching the glob-like <pattern> and their values.",
	"SET <directive> <value>",
	"    Set the configuration <directive> to <value>.",
	"RESETSTAT",
	"    Reset statistics reported by the INFO command.",
	"REWRITE",
	"    Rewrite the configuration file.")

// errNoConfigFile is Redis's answer to CONFIG REWRITE from a server started
// without a config file. Keel takes flags, so it never has one.
var errNoConfigFile = errors.New("ERR The server is running without a config file")

// cmdCONFIG implements CONFIG GET, SET, RESETSTAT, REWRITE and HELP.
//
// CONFIG GET reports the settings Keel has under Redis's parameter names, in
// Redis's units and formats, so that tools built for Redis read them
// (docs/info-compatibility.md). A setting Keel has no counterpart for, or
// gives another meaning, is not reported at all; Redis answers a name it does
// not know with nothing either. Keel has no runtime configuration yet, so
// CONFIG SET refuses every setting, in Redis's words for one it cannot set.
// RESETSTAT resets what Redis's resets that Keel reports (resetStats).
//
// The subcommand and its count have been checked against the command table by
// the time this runs; see containerCommands.
func (e *Engine) cmdCONFIG(args []string) []byte {
	if err := CommandError(&Command{Cmd: "CONFIG", Args: args}); err != nil {
		return e.encode(err, false)
	}
	switch strings.ToLower(args[0]) {
	case "help":
		return configHelp
	case "set":
		return e.encode(e.configSet(args[1:]), false)
	case "rewrite":
		return e.encode(errNoConfigFile, false)
	case "resetstat":
		e.resetStats()
		return constant.RespOk
	}
	return e.encode(e.configGet(args[1:]), false)
}

// configParameter is one setting as CONFIG GET reports it: Redis's name for
// it, and its value in Redis's format.
type configParameter struct{ name, value string }

// configParameters are the settings Keel has under Redis's parameter names.
// The engine's own come first; the transport's (port, bind, maxclients and
// the rest) only when one has said what they are, through SetServerInfo.
func (e *Engine) configParameters() []configParameter {
	limits := e.space.Limits()
	o := e.options.WithDefaults()
	appendonly := "no"
	if o.AppendOnly {
		appendonly = "yes"
	}
	// Redis writes the primary as "host port"; Keel takes it as host:port.
	replicaof := e.replicaOf()
	if i := strings.LastIndexByte(replicaof, ':'); i > 0 {
		replicaof = replicaof[:i] + " " + replicaof[i+1:]
	}
	params := []configParameter{
		{"maxmemory", strconv.FormatUint(limits.MaxMemory, 10)},
		{"maxmemory-policy", evictionPolicyName(limits.Eviction)},
		{"maxmemory-samples", strconv.Itoa(limits.EvictionSamples)},
		{"lfu-log-factor", strconv.Itoa(limits.LFULogFactor)},
		{"appendonly", appendonly},
		{"appendfilename", o.AppendFilename},
		{"appendfsync", string(o.Fsync)},
		{"auto-aof-rewrite-percentage", strconv.Itoa(e.settings.rewritePercentage)},
		{"auto-aof-rewrite-min-size", strconv.FormatInt(e.settings.rewriteMinSize, 10)},
		// slaveof is Redis's old name for replicaof, which it still answers to.
		{"replicaof", replicaof},
		{"slaveof", replicaof},
		// One database, and no snapshots: Redis's value for save turned off.
		{"databases", "1"},
		{"save", ""},
	}
	// Relative paths, the log's among them, resolve against it, as against
	// Redis's dir.
	if dir, err := os.Getwd(); err == nil {
		params = append(params, configParameter{"dir", dir})
	}
	if s := e.serverInfo; s != nil {
		maxclients := strconv.Itoa(s.MaxClients)
		params = append(params,
			configParameter{"port", strconv.Itoa(s.Port)},
			configParameter{"bind", s.Host},
			configParameter{"maxclients", maxclients},
			// Keel listens with a backlog of maxclients.
			configParameter{"tcp-backlog", maxclients},
			configParameter{"io-threads", strconv.Itoa(s.IOThreads)},
			configParameter{"hz", strconv.Itoa(s.Hz)},
			// Redis gives the password to a client that has logged in, which
			// any client asking here has.
			configParameter{"requirepass", s.RequirePass})
	}
	return params
}

// configGet answers CONFIG GET as Redis does: every parameter matching any of
// the patterns, once, as name and value pairs. A pattern with no glob
// characters is a name, matched without regard to case, and comes back
// spelled as it was asked for; a glob pattern matches without regard to case
// and brings back Redis's spelling. Redis keeps no order between parameters.
func (e *Engine) configGet(patterns []string) ReplyMap {
	params := e.configParameters()
	out := ReplyMap{}
	seen := make(map[string]bool, len(params))
	for _, pattern := range patterns {
		literal := !strings.ContainsAny(pattern, "*?[")
		lowered := strings.ToLower(pattern)
		for _, p := range params {
			if seen[p.name] {
				continue
			}
			switch {
			case literal && lowered == p.name:
				out = append(out, pattern, p.value)
			case !literal && globMatch(lowered, p.name):
				out = append(out, p.name, p.value)
			default:
				continue
			}
			seen[p.name] = true
		}
	}
	return out
}

// configSet answers CONFIG SET as Redis answers for a setting it cannot set.
// Redis names the first pair that fails: a name it does not know, or one it
// keeps immutable or, as it keeps dir by default, protected. Every setting
// Keel has is immutable to it until it has runtime configuration, and a name
// it does not report is one it does not know.
func (e *Engine) configSet(pairs []string) error {
	if len(pairs)%2 != 0 {
		return errSyntax
	}
	name := pairs[0]
	for _, p := range e.configParameters() {
		if !strings.EqualFold(name, p.name) {
			continue
		}
		reason := "can't set immutable config"
		if p.name == "dir" {
			reason = "can't set protected config"
		}
		return fmt.Errorf("ERR CONFIG SET failed (possibly related to argument '%s') - %s", name, reason)
	}
	return fmt.Errorf("ERR Unknown option or number of arguments for CONFIG SET - '%s'", name)
}
