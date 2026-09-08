package main

import (
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRequestReservationsRecoverAfterIncompleteWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("aggregate socket pressure is excluded from brief -short checks")
	}
	for _, threads := range []string{"1", "4"} {
		t.Run(threads, func(t *testing.T) {
			s := startTestServer(t, "-io-threads", threads)
			control, reader := connectTest(t, s)
			stat := func(name string) int {
				t.Helper()
				info := call(t, control, reader, "INFO", "clients")
				for _, line := range strings.Split(info, "\r\n") {
					if value, ok := strings.CutPrefix(line, name+":"); ok {
						n, err := strconv.Atoi(value)
						if err != nil {
							t.Fatal(err)
						}
						return n
					}
				}
				t.Fatalf("missing %s in %s", name, info)
				return 0
			}
			// Every writer leaves its SET incomplete, retaining input without
			// mutating storage. Buffer growth must refuse before the next backing
			// allocation crosses either the shared or input-class allowance.
			partial := "*3\r\n$3\r\nSET\r\n$7\r\npartial\r\n$12582912\r\n" + strings.Repeat("v", 3<<20)
			var writers []net.Conn
			peakInput := 0
			for i := 0; i < 64 && stat("request_allocation_refusals") == 0; i++ {
				c, _ := connectTest(t, s)
				writers = append(writers, c)
				// A refusal closes this writer; its write may see that close.
				_, _ = io.WriteString(c, partial)
				peakInput = max(peakInput, stat("retained_input_bytes"))
			}
			deadline := time.Now().Add(10 * time.Second)
			for stat("request_allocation_refusals") == 0 {
				if time.Now().After(deadline) {
					t.Fatal("request pressure did not exercise admission refusal")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if peakInput < 64<<20 {
				t.Fatalf("insufficient retained-input pressure: %d", peakInput)
			}
			if peak := stat("request_allocation_peak_bytes"); peak > 256<<20 {
				t.Fatalf("covered request admission exceeded aggregate limit: %d", peak)
			}
			if got := call(t, control, reader, "DBSIZE"); got != ":0" {
				t.Fatal(got)
			}
			for _, c := range writers {
				c.Close()
			}
			deadline = time.Now().Add(10 * time.Second)
			for stat("retained_input_bytes") > 1<<20 {
				if time.Now().After(deadline) {
					t.Fatal("incomplete inputs were not released after disconnect")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if got := call(t, control, reader, "SET", "after", "accepted"); got != "+OK" {
				t.Fatal(got)
			}
			if got := call(t, control, reader, "GET", "after"); got != "accepted" {
				t.Fatal(got)
			}
			s.stop(t)
		})
	}
}
