package core

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/data_structure"
	"github.com/stretchr/testify/require"
)

func admissionResourceSetup(t *testing.T) {
	t.Helper()
	ResetStores()
	oldMemory, oldKeys := config.MaxMemory, config.KeyNumberLimit
	config.MaxMemory, config.KeyNumberLimit = 0, 1000000
	require.NoError(t, OpenAOF(filepath.Join(t.TempDir(), "resource.aof")))
	t.Cleanup(func() { require.NoError(t, CloseAOF()); config.MaxMemory, config.KeyNumberLimit = oldMemory, oldKeys })
}

func TestAppendAdmissionBoundsRepeatedFieldsAndWideMembership(t *testing.T) {
	for _, kind := range []string{"HMGET", "SMISMEMBER"} {
		t.Run(kind, func(t *testing.T) {
			admissionResourceSetup(t)
			command := []string{kind, "key"}
			if kind == "HMGET" {
				run(t, "HSET", "key", "field", strings.Repeat("x", 64<<10))
				for i := 0; i < 32; i++ {
					command = append(command, "field")
				}
			} else {
				for i := 0; i < 1000; i++ {
					command = append(command, "absent")
				}
			}
			logBound, replyBound, logUsed, replyUsed, ok := admissionRun(t, command)
			require.True(t, ok)
			require.LessOrEqual(t, logUsed, logBound)
			require.LessOrEqual(t, replyUsed, replyBound, "repeated fields and membership replies grow with argument count")
		})
	}
}

func TestAppendAdmissionRejectsCollectionGrowthThatWouldEvict(t *testing.T) {
	for _, kind := range []string{"ZADD", "RPUSH"} {
		t.Run(kind, func(t *testing.T) {
			admissionResourceSetup(t)
			run(t, "SET", "resident", "v")
			command := &Command{Cmd: "ZADD", Args: []string{"new-zset", "1", "member"}}
			room := uint64(500)
			if kind == "RPUSH" {
				args := []string{"list"}
				for i := 0; i < 1024; i++ {
					args = append(args, "v")
				}
				run(t, "RPUSH", args...)
				command = &Command{Cmd: "RPUSH", Args: []string{"list", "v"}}
				room = 1024
			}
			config.MaxMemory = data_structure.TotalMemUsed() + room
			_, _, ok := AppendAdmission([]*Command{command})
			require.False(t, ok, "new structure metadata and ring-capacity growth must be reserved before concurrent execution")
		})
	}
}
