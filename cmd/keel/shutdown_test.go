package main

import (
	"bytes"
	"errors"
	"log"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestShutdownWaitReturnsCleanupResult(t *testing.T) {
	for _, want := range []error{nil, errors.New("persistence sync failed")} {
		done := make(chan error, 1)
		done <- want
		require.Equal(t, want, waitForShutdown(done, make(chan os.Signal), time.Minute))
	}
}

func TestShutdownWaitHonorsSecondSignal(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	require.EqualError(t, waitForShutdown(make(chan error), signals, time.Minute), "second termination signal")
}

func TestShutdownWaitUsesConfiguredDeadlineAndCapturesPhase(t *testing.T) {
	previous := log.Writer()
	var output bytes.Buffer
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	start := time.Now()
	err := waitForShutdown(make(chan error), make(chan os.Signal), time.Millisecond)
	require.EqualError(t, err, "shutdown exceeded 1ms")
	require.Less(t, time.Since(start), 4*time.Second, "must not retain the former five-second fixed timer")
	require.Contains(t, output.String(), "shutdown timeout goroutine dump")
	require.Contains(t, output.String(), "waitForShutdown")
	require.Less(t, output.Len(), (1<<20)+1024)
}
