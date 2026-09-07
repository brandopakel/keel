package main

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type deadlineRecorder struct {
	net.Conn
	read, write time.Time
	err         error
}

func (c *deadlineRecorder) SetDeadline(at time.Time) error {
	if c.err != nil {
		return c.err
	}
	c.read, c.write = at, at
	return nil
}
func (c *deadlineRecorder) SetReadDeadline(at time.Time) error {
	if c.err != nil {
		return c.err
	}
	c.read = at
	return nil
}
func (c *deadlineRecorder) SetWriteDeadline(at time.Time) error {
	if c.err != nil {
		return c.err
	}
	c.write = at
	return nil
}
func (c *deadlineRecorder) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *deadlineRecorder) Write(b []byte) (int, error) { return len(b), nil }

func TestIdleConnPreservesDirectionalDeadlines(t *testing.T) {
	for _, direction := range []string{"read", "write", "both"} {
		t.Run(direction, func(t *testing.T) {
			raw := &deadlineRecorder{}
			conn := &idleConn{Conn: raw, idle: time.Second}
			explicit := time.Unix(1, 0)
			switch direction {
			case "read":
				require.NoError(t, conn.SetReadDeadline(explicit))
			case "write":
				require.NoError(t, conn.SetWriteDeadline(explicit))
			case "both":
				require.NoError(t, conn.SetDeadline(explicit))
			}
			_, err := conn.Read(nil)
			require.ErrorIs(t, err, io.EOF)
			_, err = conn.Write(nil)
			require.NoError(t, err)
			if direction != "write" {
				require.Equal(t, explicit, raw.read)
			} else {
				require.True(t, raw.read.After(explicit))
			}
			if direction != "read" {
				require.Equal(t, explicit, raw.write)
			} else {
				require.True(t, raw.write.After(explicit))
			}
		})
	}
}

func TestFailedExplicitDeadlineKeepsIdleRefreshEnabled(t *testing.T) {
	for _, direction := range []string{"read", "write", "both"} {
		t.Run(direction, func(t *testing.T) {
			raw := &deadlineRecorder{err: errors.New("deadline failed")}
			conn := &idleConn{Conn: raw, idle: time.Second}
			explicit := time.Unix(1, 0)
			switch direction {
			case "read":
				require.Error(t, conn.SetReadDeadline(explicit))
			case "write":
				require.Error(t, conn.SetWriteDeadline(explicit))
			case "both":
				require.Error(t, conn.SetDeadline(explicit))
			}
			raw.err = nil
			_, err := conn.Read(nil)
			require.ErrorIs(t, err, io.EOF)
			_, err = conn.Write(nil)
			require.NoError(t, err)
			require.True(t, raw.read.After(explicit))
			require.True(t, raw.write.After(explicit))
		})
	}
}
