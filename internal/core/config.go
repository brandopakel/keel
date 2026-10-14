package core

import (
	"errors"
	"log"
	"os"
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
// GET and SET answer from Redis's own table of directives (config_specs.go,
// generated from Redis 8.10.1's src/config.c), for the directives Keel
// answers for (config_keel.go): its own settings, read and changed live; the
// settings that only tune Redis's internals, accepted, kept and reported
// without effect; and the settings Keel cannot change, reported and refused
// as Redis refuses a directive it cannot set. Every other name is answered
// as Redis answers one it does not know. docs/info-compatibility.md lists
// them. RESETSTAT resets what Redis's resets that Keel reports
// (resetStats).
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
		if err := e.configSet(args[1:]); err != nil {
			return e.encode(err, false)
		}
		return constant.RespOk
	case "rewrite":
		return e.encode(errNoConfigFile, false)
	case "resetstat":
		e.resetStats()
		return constant.RespOk
	}
	return e.encode(e.configGet(args[1:]), false)
}

// configState is what CONFIG SET has kept of the directives Keel accepts
// without effect: each one set, by Redis's name, as CONFIG GET writes it.
type configState struct {
	stored map[string]string
}

// keelConfigFor is how Keel answers s, or nil when it answers s as a name it
// does not know: one it has no handling for, or a transport's setting with no
// transport to say what it is.
func (e *Engine) keelConfigFor(s *configSpec) *keelConfig {
	configTables()
	k, ok := keelConfigs[s.name]
	if !ok || (k.transport && e.serverInfo == nil) {
		return nil
	}
	return &k
}

// configValue is s's value in Keel, as CONFIG GET writes it.
func (e *Engine) configValue(s *configSpec, k *keelConfig) string {
	if k.handling == configStored {
		return e.storedConfig(s.name)
	}
	return k.get(e)
}

// storedConfig is a directive Keel keeps without effect: the value last set,
// or Redis's default.
func (e *Engine) storedConfig(name string) string {
	if e.config != nil {
		if v, ok := e.config.stored[name]; ok {
			return v
		}
	}
	if s := lookupConfig(name); s != nil {
		return s.def
	}
	return ""
}

// configGet answers CONFIG GET as Redis's configGetCommand does: for each
// pattern, a name with no glob characters is looked up without regard to
// case and comes back spelled as it was asked for; a glob pattern is matched
// without regard to case against every name and alias, except hidden ones,
// and brings back Redis's spelling. Each name comes back once. Redis keeps no
// order between them; Keel gives its table's.
func (e *Engine) configGet(patterns []string) ReplyMap {
	out := ReplyMap{}
	seen := map[string]bool{}
	add := func(key string, s *configSpec, k *keelConfig) {
		out = append(out, key, e.configValue(s, k))
		seen[strings.ToLower(key)] = true
	}
	for _, pattern := range patterns {
		if !strings.ContainsAny(pattern, "[*?") {
			if seen[strings.ToLower(pattern)] {
				continue
			}
			if s := lookupConfig(pattern); s != nil {
				if k := e.keelConfigFor(s); k != nil {
					add(pattern, s, k)
				}
			}
			continue
		}
		lowered := strings.ToLower(pattern)
		for i := range redisConfigSpecs {
			s := &redisConfigSpecs[i]
			if s.flags&configHidden != 0 || !s.built() {
				continue
			}
			k := e.keelConfigFor(s)
			if k == nil {
				continue
			}
			for _, key := range [2]string{s.name, s.alias} {
				if key != "" && !seen[strings.ToLower(key)] && globMatch(lowered, key) {
					add(key, s, k)
				}
			}
		}
	}
	return out
}

// configSetError is Redis's error for a CONFIG SET that failed for name,
// with its reason.
func configSetError(name, reason string) error {
	if reason == "" {
		return errors.New("ERR CONFIG SET failed (possibly related to argument '" + name + "')")
	}
	return errors.New("ERR CONFIG SET failed (possibly related to argument '" + name + "') - " + reason)
}

// configSet answers CONFIG SET as Redis's configSetCommand does: every pair
// or none.
//
// Every name is looked up first. The first one Redis does not know, or Keel
// does not answer for, fails the command, unless a pair before it already
// had; so does a directive Redis cannot set at all (immutable, or protected,
// as enable-protected-configs leaves dir), whether Keel answers for it or
// not, or one given twice, under its name or its alias. Then each value is read, in order, as Redis reads it, and the
// first that does not read fails the command, named as Redis spells the
// directive. Only then is anything changed, in order; a change that fails
// puts back every one made before it, and fails the command.
func (e *Engine) configSet(args []string) error {
	if len(args)%2 != 0 {
		return errSyntax
	}
	n := len(args) / 2
	specs := make([]*configSpec, n)
	keels := make([]*keelConfig, n)
	names := make([]string, n)
	var unknown, failed, reason string
	invalid := false
	for i := 0; i < n; i++ {
		arg := args[2*i]
		s := lookupConfig(arg)
		var k *keelConfig
		if s != nil {
			k = e.keelConfigFor(s)
		}
		// A directive Redis cannot set at all is refused in Redis's words
		// whether or not Keel answers for it: no value of it could be taken.
		if s == nil || (k == nil && s.flags&(configImmutable|configProtected) == 0) {
			if !invalid {
				unknown, invalid = arg, true
			}
			continue
		}
		if invalid {
			continue
		}
		if s.flags&(configImmutable|configProtected) != 0 {
			reason = errImmutableConfig
			if s.flags&configImmutable == 0 {
				reason = "can't set protected config"
			}
			failed, invalid = arg, true
			continue
		}
		for j := 0; j < i; j++ {
			if specs[j] == s {
				reason, failed, invalid = "duplicate parameter", arg, true
				break
			}
		}
		specs[i], keels[i] = s, k
		// The directive as Redis spells it: the alias, if that is what was
		// asked for.
		names[i] = s.name
		if s.alias != "" && strings.EqualFold(arg, s.alias) {
			names[i] = s.alias
		}
	}
	switch {
	case unknown != "":
		return errors.New("ERR Unknown option or number of arguments for CONFIG SET - '" + unknown + "'")
	case invalid:
		return configSetError(failed, reason)
	}

	values := make([]string, n)
	read := 0
	// A directive as this command leaves it so far, for the checks that read
	// one against another.
	setting := func(name string) string {
		for j := 0; j < read; j++ {
			if specs[j].name == name {
				return values[j]
			}
		}
		if s := lookupConfig(name); s != nil {
			if k := e.keelConfigFor(s); k != nil {
				return e.configValue(s, k)
			}
		}
		return ""
	}
	for i := 0; i < n; i++ {
		s, k := specs[i], keels[i]
		var v, why string
		if k.read != nil {
			v, why = k.read(args[2*i+1])
		} else {
			v, why = s.read(args[2*i+1])
		}
		if why == "" && k.valid != nil {
			why = k.valid(setting, v)
		}
		if why == "" && k.handling == configFixed && v != k.get(e) {
			// Keel's value of it cannot change: any other value is refused as
			// Redis refuses a directive it cannot set.
			why = errImmutableConfig
		}
		if why != "" {
			return configSetError(names[i], why)
		}
		values[i] = v
		read++
	}

	old := make([]string, n)
	for i := 0; i < n; i++ {
		old[i] = e.configValue(specs[i], keels[i])
	}
	for i := 0; i < n; i++ {
		if why := e.applyConfig(specs[i], keels[i], values[i]); why != "" {
			for j := i - 1; j >= 0; j-- {
				// Putting a value back can fail too, the file limit having
				// moved under maxclients, say; Redis logs it
				// (restoreBackupConfig), and so does Keel.
				if again := e.applyConfig(specs[j], keels[j], old[j]); again != "" {
					log.Printf("CONFIG SET: failed restoring %s to '%s': %s", specs[j].name, old[j], again)
				}
			}
			return configSetError(names[i], why)
		}
	}
	return nil
}

// applyConfig sets one directive to a value already read, and returns the
// error text if it could not.
func (e *Engine) applyConfig(s *configSpec, k *keelConfig, v string) string {
	switch k.handling {
	case configLive:
		return k.set(e, v)
	case configStored:
		if e.config == nil {
			e.config = &configState{stored: map[string]string{}}
		}
		e.config.stored[s.name] = v
	}
	return ""
}

// workingDirectory is dir: relative paths, the log's among them, resolve
// against it, as against Redis's dir.
func workingDirectory(*Engine) string {
	dir, _ := os.Getwd()
	return dir
}
