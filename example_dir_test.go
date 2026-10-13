package keel_test

import (
	"os"
	"testing"
)

// exampleDir is a directory for an example's files, removed once the tests
// have run: examples have no t.TempDir.
func exampleDir() string {
	dir, err := os.MkdirTemp("", "keel-example-")
	if err != nil {
		panic(err)
	}
	exampleDirs = append(exampleDirs, dir)
	return dir
}

var exampleDirs []string

func TestMain(m *testing.M) {
	if path := os.Getenv(crashChildEnv); path != "" {
		crashChild(path, os.Getenv(crashChildFsync))
		return
	}
	code := m.Run()
	for _, dir := range exampleDirs {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}
