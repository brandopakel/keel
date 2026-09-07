package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/brandopakel/keel/internal/config"
	"github.com/brandopakel/keel/internal/core"
)

type replicaUpdate struct {
	frame   core.ReplicationFrame
	applied chan error
}

// The transport never accesses the keyspace. One frame at a time crosses to
// the event loop, and the cursor advances only after successful application.
func startReplicaTransport() (<-chan replicaUpdate, func()) {
	updates := make(chan replicaUpdate, 1)
	if config.ReplicaOf == "" {
		return updates, func() {}
	}
	address, password, useTLS := config.ReplicaOf, config.ReplicaPassword, config.ReplicaTLS
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	protocol := config.ReplicationProtocol
	initialEpoch, initialOffset := core.ReplicaResumeCursor()
	go func() {
		defer close(done)
		epoch, offset := initialEpoch, initialOffset
		snapshotID := ""
		var snapshotOffset uint64
		var lastErrorLog time.Time
		for ctx.Err() == nil {
			dialer := net.Dialer{Timeout: 2 * time.Second}
			conn, err := dialer.DialContext(ctx, "tcp", address)
			if err == nil {
				disconnected := make(chan struct{})
				raw := conn
				go func() {
					select {
					case <-ctx.Done():
						raw.Close()
					case <-disconnected:
					}
				}()
				if useTLS {
					host, _, _ := net.SplitHostPort(address)
					secure := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
					conn = secure
				}
				reader := bufio.NewReader(conn)
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				_, err = replicaExchange(conn, reader, []string{"AUTH", password})
				for err == nil && ctx.Err() == nil {
					conn.SetDeadline(time.Now().Add(3 * time.Second))
					var body []byte
					parts := []string{"KEEL.REPL.PULL", epoch, strconv.FormatUint(offset, 10)}
					if protocol == 2 {
						parts = []string{"KEEL.REPL.PULL2", epoch, strconv.FormatUint(offset, 10), snapshotID, strconv.FormatUint(snapshotOffset, 10)}
						// Term-zero traffic keeps the original protocol-2 request
						// shape so rolling upgrades can exchange unchanged frames.
						if term := core.CurrentTerm(); term != 0 {
							parts = append(parts, strconv.FormatUint(term, 10))
						}
					}
					body, err = replicaExchange(conn, reader, parts)
					if protocol == 2 && len(parts) == 5 && errors.Is(err, errReplicationTermRequired) {
						// A term-zero new replica can discover a promoted primary
						// without sending new syntax to old term-zero primaries.
						parts = append(parts, strconv.FormatUint(core.CurrentTerm(), 10))
						body, err = replicaExchange(conn, reader, parts)
					}
					if err != nil {
						break
					}
					var frame core.ReplicationFrame
					if err = json.Unmarshal(body, &frame); err != nil {
						break
					}
					update := replicaUpdate{frame: frame, applied: make(chan error, 1)}
					select {
					case updates <- update:
						wake()
					case <-ctx.Done():
						err = ctx.Err()
					}
					if err != nil {
						break
					}
					select {
					case err = <-update.applied:
					case <-ctx.Done():
						err = ctx.Err()
					}
					if err != nil {
						break
					}
					if protocol == 2 && frame.Full {
						if frame.Pending {
							epoch, offset, snapshotID, snapshotOffset = "", 0, "", 0
						} else if frame.SnapshotDone {
							epoch, offset, snapshotID, snapshotOffset = frame.Epoch, frame.To, "", 0
						} else {
							epoch, snapshotID, snapshotOffset = frame.Epoch, frame.SnapshotID, frame.SnapshotOffset+uint64(len(frame.Body))
						}
					} else {
						epoch, offset = frame.Epoch, frame.To
					}
					if protocol == 2 && !frame.Pending && (frame.Full || !frame.CaughtUp) {
						continue
					}
					select {
					case <-time.After(100 * time.Millisecond):
					case <-ctx.Done():
					}
				}
				conn.Close()
				close(disconnected)
			}
			if err != nil && time.Since(lastErrorLog) >= 30*time.Second {
				log.Printf("replication protocol %d: %v", protocol, err)
				lastErrorLog = time.Now()
			}
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}()
	return updates, func() { cancel(); <-done }
}

var errReplicationTermRequired = errors.New("primary requires a term-aware protocol 2 request")

func replicaExchange(conn net.Conn, reader *bufio.Reader, parts []string) ([]byte, error) {
	body := core.Encode(parts, false)
	if n, err := conn.Write(body); err != nil {
		return nil, err
	} else if n != len(body) {
		return nil, io.ErrShortWrite
	}
	rawLine, err := reader.ReadSlice('\n')
	line := string(rawLine)
	if err != nil {
		return nil, err
	}
	if len(line) > 65536 || !strings.HasSuffix(line, "\r\n") {
		return nil, fmt.Errorf("invalid primary reply")
	}
	if line == "+OK\r\n" {
		return []byte("OK"), nil
	}
	if line == core.ReplicationTermRequiredReply {
		return nil, errReplicationTermRequired
	}
	if !strings.HasPrefix(line, "$") {
		return nil, fmt.Errorf("primary rejected replication request")
	}
	n, err := strconv.Atoi(line[1 : len(line)-2])
	if err != nil || n < 0 || n > 16<<20 {
		return nil, fmt.Errorf("primary reply exceeds limit")
	}
	payload := make([]byte, n+2)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return nil, err
	}
	if payload[n] != '\r' || payload[n+1] != '\n' {
		return nil, fmt.Errorf("invalid primary bulk reply")
	}
	return payload[:n], nil
}
