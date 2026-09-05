package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
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
	go func() {
		defer close(done)
		epoch := ""
		var offset uint64
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
					body, err = replicaExchange(conn, reader, []string{"KEEL.REPL.PULL", epoch, strconv.FormatUint(offset, 10)})
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
					epoch, offset = frame.Epoch, frame.To
					select {
					case <-time.After(100 * time.Millisecond):
					case <-ctx.Done():
					}
				}
				conn.Close()
				close(disconnected)
			}
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
			}
		}
	}()
	return updates, func() { cancel(); <-done }
}

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
