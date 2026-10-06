package core

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func admissionResourceSetup(t *testing.T, e *Engine) {
	t.Helper()
	e.resetStores()
	withOptionsOn(t, e, func(o *Options) { o.MaxMemory, o.MaxKeys = 0, 1000000 })
	require.NoError(t, e.OpenAOF(filepath.Join(t.TempDir(), "resource.aof")))
	t.Cleanup(func() { require.NoError(t, e.CloseAOF()) })
}

func TestAppendAdmissionBoundsRepeatedFieldsAndWideMembership(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"HMGET", "SMISMEMBER"} {
		t.Run(kind, func(t *testing.T) {
			admissionResourceSetup(t, e)
			command := []string{kind, "key"}
			if kind == "HMGET" {
				runOn(t, e, "HSET", "key", "field", strings.Repeat("x", 64<<10))
				for i := 0; i < 32; i++ {
					command = append(command, "field")
				}
			} else {
				for i := 0; i < 1000; i++ {
					command = append(command, "absent")
				}
			}
			logBound, replyBound, logUsed, replyUsed, ok := admissionRun(t, e, command)
			require.True(t, ok)
			require.LessOrEqual(t, logUsed, logBound)
			require.LessOrEqual(t, replyUsed, replyBound, "repeated fields and membership replies grow with argument count")
		})
	}
}

func TestAppendAdmissionRejectsCollectionGrowthThatWouldEvict(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	for _, kind := range []string{"ZADD", "RPUSH"} {
		t.Run(kind, func(t *testing.T) {
			admissionResourceSetup(t, e)
			runOn(t, e, "SET", "resident", "v")
			command := &Command{Cmd: "ZADD", Args: []string{"new-zset", "1", "member"}}
			room := uint64(500)
			if kind == "RPUSH" {
				args := []string{"list"}
				for i := 0; i < 1024; i++ {
					args = append(args, "v")
				}
				runOn(t, e, "RPUSH", args...)
				command = &Command{Cmd: "RPUSH", Args: []string{"list", "v"}}
				room = 1024
			}
			reconfigure(t, e, func(o *Options) { o.MaxMemory = e.space.TotalMemUsed() + room })
			_, _, ok := e.AppendAdmission([]*Command{command})
			require.False(t, ok, "new structure metadata and ring-capacity growth must be reserved before concurrent execution")
		})
	}
}
