package config

import "runtime/debug"

// Version is the build's version string.
//
// Set by the linker for a release binary, which is where the tag lives:
//
//	go build -ldflags "-X github.com/brandopakel/keel/internal/config.Version=v0.1.0"
//
// Left empty for every other build, and filled in from the module's own build
// information instead. That is what makes `go install ...@latest` report
// something truthful rather than "dev": the toolchain records the version it
// resolved, so a binary installed from a tag says the tag and one installed
// from a branch says the pseudo-version, which names the commit.
var Version = ""

// BuildVersion returns the version to report, preferring the linker's value.
func BuildVersion() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		// A plain `go build` in a working tree records nothing useful, and
		// saying so is better than inventing a number.
		return "devel"
	}
	if info.Main.Version == "(devel)" {
		return "devel"
	}
	return info.Main.Version
}

// BuildRevision returns the commit the binary was built from, when the
// toolchain recorded one. Empty otherwise.
func BuildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			if len(s.Value) > 12 {
				return s.Value[:12]
			}
			return s.Value
		}
	}
	return ""
}
