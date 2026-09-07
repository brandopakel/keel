package server

import (
	"errors"
	"testing"

	"github.com/brandopakel/keel/internal/core/io_multiplexing"
	"github.com/stretchr/testify/require"
)

type countedMonitor struct {
	io_multiplexing.IOMultiplexer
	events []io_multiplexing.Event
	err    error
}

func (m *countedMonitor) Monitor(e io_multiplexing.Event) error {
	m.events = append(m.events, e)
	return m.err
}

func TestReadinessPreservesTransitionsAndRetriesFailures(t *testing.T) {
	m := &countedMonitor{}
	c := &client{fd: 42}
	for _, op := range []io_multiplexing.Operation{
		io_multiplexing.OpRead, io_multiplexing.OpRead,
		io_multiplexing.OpWrite, io_multiplexing.OpWrite,
		io_multiplexing.OpNone, io_multiplexing.OpNone, io_multiplexing.OpRead,
	} {
		require.NoError(t, c.setInterest(m, op))
	}
	require.Equal(t, []io_multiplexing.Event{{Fd: 42, Op: io_multiplexing.OpRead},
		{Fd: 42, Op: io_multiplexing.OpWrite}, {Fd: 42, Op: io_multiplexing.OpNone},
		{Fd: 42, Op: io_multiplexing.OpRead}}, m.events)
	m.err = errors.New("registration failed")
	require.Error(t, c.setInterest(m, io_multiplexing.OpNone))
	m.err = nil
	require.NoError(t, c.setInterest(m, io_multiplexing.OpNone))
	require.Len(t, m.events, 6, "a failed change must not be cached as successful")
	// A new connection using a recycled descriptor needs its own registration.
	replacement := &client{fd: 42}
	require.NoError(t, replacement.setInterest(m, io_multiplexing.OpNone))
	require.Len(t, m.events, 7)
}
