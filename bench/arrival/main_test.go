package main

import (
	"bufio"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type pipelineWriteProbe struct {
	net.Conn
	bytes, largest int
}

func (p *pipelineWriteProbe) SetDeadline(time.Time) error { return nil }
func (p *pipelineWriteProbe) Write(b []byte) (int, error) {
	p.largest = max(p.largest, len(b))
	n := max(1, len(b)/2) // force the partial-write path too
	p.bytes += n
	return n, nil
}

func TestLargePipelineDoesNotAllocateWholeBatch(t *testing.T) {
	request := []byte(strings.Repeat("x", 16300))
	reader := bufio.NewReader(strings.NewReader(strings.Repeat("+OK\r\n", 4096)))
	conn := new(pipelineWriteProbe)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := exchangePipeline(conn, reader, request, time.Second, 4096)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if conn.bytes != len(request)*4096 || conn.largest > 64<<10 {
		t.Fatalf("bytes=%d largest chunk=%d", conn.bytes, conn.largest)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<10 {
		t.Fatalf("large pipeline allocated %d bytes", allocated)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Fatalf("replies not completely consumed: %v", err)
	}
}

func TestHistogramQuantilesBoundSamples(t *testing.T) {
	for n := int64(1); n < 1e12; n = n*3 + 1 {
		var h histogram
		h.add(time.Duration(n))
		upper := h.percentile(.99) * 1e6
		if upper < float64(n) || upper > float64(n)*1.016+1 {
			t.Fatalf("sample %d bound %f", n, upper)
		}
	}
	var a, b histogram
	a.add(time.Millisecond)
	b.add(2 * time.Millisecond)
	a.merge(&b)
	if a.count != 2 || a.percentile(.99) < 2 || a.max != 2*time.Millisecond {
		t.Fatal(a.summary())
	}
}

func TestReplyFramingRejectsTruncationAndInvalidTerminator(t *testing.T) {
	for _, raw := range []string{"+OK\r\n", ":42\r\n", "$-1\r\n", "$4\r\na\x00bc\r\n", "*2\r\n$0\r\n\r\n:1\r\n"} {
		if _, err := readReply(bufio.NewReader(strings.NewReader(raw)), 0); err != nil {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"+OK\n", "$4\r\nabc", "$1\r\nxZZ", "*1\r\n", "$67108865\r\n", "-ERR refused\r\n"} {
		if _, err := readReply(bufio.NewReader(strings.NewReader(raw)), 0); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestScheduledLoadAccountsForOverloadWithoutWaitingForReplies(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var server sync.WaitGroup
	server.Add(1)
	go func() {
		defer server.Done()
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		for {
			if _, err := readReply(r, 0); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
			if _, err := io.WriteString(c, "$1\r\nx\r\n"); err != nil {
				return
			}
		}
	}()
	o := options{Address: listener.Addr().String(), Rate: 200, Seconds: 1, Connections: 1, Queue: 2, Keys: 1, Size: 1, Timeout: time.Second}
	report, err := measure(o)
	if err != nil {
		t.Fatal(err)
	}
	server.Wait()
	if report["scheduled"].(int) != 200 || report["failed"].(uint64) != 0 || report["queue_dropped"].(uint64) == 0 {
		t.Fatal(report)
	}
	accounted := report["completed"].(uint64) + report["queue_dropped"].(uint64) + report["queue_expired"].(uint64)
	if accounted != 200 {
		t.Fatal(report)
	}
	if report["scheduled_latency"].(map[string]any)["p99_ms"].(float64) < 20 {
		t.Fatal("scheduled latency omitted service or queue time", report)
	}
}

func TestExpiryCohortsDoNotRefreshBeforeTheirDeadline(t *testing.T) {
	o := options{Prefix: "test:", Keys: 1, CohortExpiry: true}
	start := time.Unix(100, 0)
	first := requestKey(o, 0, start)
	for second := 1; second < 4; second++ {
		if requestKey(o, 0, start.Add(time.Duration(second)*time.Second)) == first {
			t.Fatal("cohort reused before expiry")
		}
	}
	if requestKey(o, 0, start.Add(4*time.Second)) != first {
		t.Fatal("cohort namespace must remain bounded")
	}
}

func TestReadWriteChoiceDoesNotPartitionTheKeyspace(t *testing.T) {
	reads, writes := make([]bool, 100), make([]bool, 100)
	for i := 0; i < 10000; i++ {
		k := keyIndex(uint64(i), 100)
		if i%100 < 50 {
			writes[k] = true
		} else {
			reads[k] = true
		}
	}
	for key := range reads {
		if !reads[key] || !writes[key] {
			t.Fatalf("key %d did not receive both reads and writes", key)
		}
	}
}

// Large collection replies must not allocate one discard reader per member;
// otherwise the load generator, rather than the server, sets the plateau.
func BenchmarkDiscardLargeCollectionReply(b *testing.B) {
	payload := "*8192\r\n" + strings.Repeat("$64\r\n"+strings.Repeat("x", 64)+"\r\n", 8192)
	src := strings.NewReader(payload)
	r := bufio.NewReaderSize(src, 16<<10)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		src.Reset(payload)
		r.Reset(src)
		if _, err := readReply(r, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPipelineExchangeDrainsEveryReplyAndRefusesOversizedRequest(t *testing.T) {
	for _, complete := range []bool{true, false} {
		client, server := net.Pipe()
		done := make(chan error, 1)
		go func() {
			defer server.Close()
			r := bufio.NewReader(server)
			for i := 0; i < 3; i++ {
				if _, err := readReply(r, 0); err != nil {
					done <- err
					return
				}
			}
			replies := 2
			if complete {
				replies = 3
			}
			_, err := io.WriteString(server, strings.Repeat("+PONG\r\n", replies))
			done <- err
		}()
		err := exchangePipeline(client, bufio.NewReader(client), wire("PING"), time.Second, 3)
		client.Close()
		if (err == nil) != complete {
			t.Fatalf("complete=%t error=%v", complete, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	if err := exchangePipeline(client, bufio.NewReader(client), make([]byte, 17<<10), time.Second, 4096); err == nil {
		t.Fatal("accepted oversized batch")
	}
}

func TestLargeCollectionDiscardDoesNotAllocatePerMember(t *testing.T) {
	payload := "*8192\r\n" + strings.Repeat("$64\r\n"+strings.Repeat("x", 64)+"\r\n", 8192)
	src := strings.NewReader(payload)
	r := bufio.NewReaderSize(src, 16<<10)
	invalid := false
	allocs := testing.AllocsPerRun(10, func() {
		src.Reset(payload)
		r.Reset(src)
		kind, err := readReply(r, 0)
		if err != nil || kind != '*' || r.Buffered() != 0 || src.Len() != 0 {
			invalid = true
		}
	})
	if invalid {
		t.Fatal("did not consume the complete array reply")
	}
	if allocs > 1 {
		t.Fatalf("per-member discard allocations regressed: %v", allocs)
	}
}

func TestScheduledStartRejectsStaleEpochAndKeepsFutureOffset(t *testing.T) {
	now := time.Now()
	for _, start := range []int64{now.Add(-time.Second).UnixNano(), now.UnixNano(), -1} {
		if _, err := scheduledStart(start, now); err == nil {
			t.Fatal("accepted stale start", start)
		}
	}
	future, err := scheduledStart(now.Add(time.Second).UnixNano(), now)
	if err != nil || future.Sub(now) != time.Second {
		t.Fatal(future, err)
	}
	immediate, err := scheduledStart(0, now)
	if err != nil || !immediate.Equal(now) {
		t.Fatal(immediate, err)
	}
	if _, err := measure(options{StartNS: now.Add(-time.Second).UnixNano()}); err == nil {
		t.Fatal("stale measurement must fail before dialing")
	}
}
