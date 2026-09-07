package main

import (
	"bufio"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

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
