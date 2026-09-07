// arrival measures scheduled-arrival latency, including generator and queue
// delay. Bounded admission drops are reported instead of silently reducing load.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/bits"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const maxReplyElements = 1 << 20

func maxCollectionKeys(kind string) int {
	if kind == "hash" || kind == "zset" {
		return maxReplyElements / 2 // field/value or member/score pairs
	}
	return 1000000
}

type histogram struct {
	buckets [4096]uint64
	count   uint64
	max     time.Duration
}

func (h *histogram) add(d time.Duration) {
	if d < 0 {
		d = 0
	}
	n := max(uint64(d), 1)
	e := bits.Len64(n) - 1
	var sub uint64
	if e >= 6 {
		sub = (n >> (e - 6)) - 64
	} else {
		sub = (n << (6 - e)) - 64
	}
	h.buckets[e*64+int(sub)]++
	h.count++
	h.max = max(h.max, d)
}
func (h *histogram) merge(other *histogram) {
	for i, n := range other.buckets {
		h.buckets[i] += n
	}
	h.count += other.count
	h.max = max(h.max, other.max)
}
func (h *histogram) percentile(f float64) float64 {
	if h.count == 0 {
		return 0
	}
	target := max(uint64(float64(h.count)*f+.999999), 1)
	var seen uint64
	for i, n := range h.buckets {
		seen += n
		if seen < target {
			continue
		}
		e, sub := i/64, uint64(i%64)
		upper := sub + 65
		if e >= 6 {
			upper <<= e - 6
		} else {
			upper = (upper + (1 << uint(6-e)) - 1) >> uint(6-e)
		}
		return float64(upper) / 1e6
	}
	return float64(h.max) / 1e6
}
func (h *histogram) summary() map[string]any {
	return map[string]any{"count": h.count, "p50_ms": h.percentile(.5), "p99_ms": h.percentile(.99), "p999_ms": h.percentile(.999), "max_ms": float64(h.max) / 1e6}
}

type options struct {
	Address      string        `json:"address"`
	Rate         int           `json:"scheduled_rate"`
	Seconds      float64       `json:"seconds"`
	Connections  int           `json:"connections"`
	Pipeline     int           `json:"pipeline"`
	Queue        int           `json:"queue_slots"`
	Keys         int           `json:"keys"`
	Size         int           `json:"value_bytes"`
	Writes       int           `json:"write_percent"`
	Prefix       string        `json:"prefix"`
	CohortExpiry bool          `json:"cohort_expiry"`
	Collection   string        `json:"collection"`
	Timeout      time.Duration `json:"request_timeout_ns"`
	StartNS      int64         `json:"scheduled_start_unix_ns"`
}

func wire(parts ...string) []byte {
	b := make([]byte, 0, 64)
	b = append(b, '*')
	b = strconv.AppendInt(b, int64(len(parts)), 10)
	b = append(b, '\r', '\n')
	for _, p := range parts {
		b = append(b, '$')
		b = strconv.AppendInt(b, int64(len(p)), 10)
		b = append(b, '\r', '\n')
		b = append(b, p...)
		b = append(b, '\r', '\n')
	}
	return b
}

// readReply validates framing while discarding bounded payloads. Correctness
// validation of values is performed separately by the differential harness.
func readReply(r *bufio.Reader, depth int) (byte, error) {
	if depth > 8 {
		return 0, errors.New("nested reply limit")
	}
	line, err := r.ReadSlice('\n')
	if err != nil {
		return 0, err
	}
	if len(line) < 3 || line[len(line)-2] != '\r' {
		return 0, errors.New("invalid reply header")
	}
	kind := line[0]
	if kind == '-' {
		return kind, fmt.Errorf("server error: %s", line[1:len(line)-2])
	}
	if kind == '+' {
		return kind, nil
	}
	n, err := strconv.ParseInt(string(line[1:len(line)-2]), 10, 64)
	if err != nil {
		return 0, err
	}
	if kind == ':' {
		return kind, nil
	}
	if n == -1 && (kind == '$' || kind == '*') {
		return kind, nil
	}
	if n < 0 || n > 64<<20 {
		return 0, errors.New("reply length limit")
	}
	if kind == '$' {
		// Discard in-place: io.CopyN creates one LimitedReader per member.
		if _, err := r.Discard(int(n)); err != nil {
			return 0, err
		}
		a, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		if a != '\r' || b != '\n' {
			return 0, errors.New("invalid bulk terminator")
		}
		return kind, nil
	}
	if kind == '*' && n <= maxReplyElements {
		for i := int64(0); i < n; i++ {
			if _, err := readReply(r, depth+1); err != nil {
				return 0, err
			}
		}
		return kind, nil
	}
	return 0, errors.New("unsupported reply")
}

func exchange(c net.Conn, r *bufio.Reader, request []byte, timeout time.Duration) error {
	return exchangePipeline(c, r, request, timeout, 1)
}

func exchangePipeline(c net.Conn, r *bufio.Reader, request []byte, timeout time.Duration, pipeline int) error {
	if pipeline < 1 || pipeline > 4096 || len(request) > (64<<20)/pipeline {
		return errors.New("pipeline request size limit")
	}
	if err := c.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	// Keep small pipelines in one write without multiplying a large request
	// into a separate 64 MiB buffer for every connection. The original command
	// remains immutable; repeated storage is at most 64 KiB per exchange.
	perChunk := min(pipeline, max(1, (64<<10)/max(1, len(request))))
	chunk := request
	if perChunk > 1 {
		chunk = bytes.Repeat(request, perChunk)
	}
	for sent := 0; sent < pipeline; {
		count := min(perChunk, pipeline-sent)
		for remaining := chunk[:count*len(request)]; len(remaining) > 0; {
			n, err := c.Write(remaining)
			if err != nil {
				return err
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			remaining = remaining[n:]
		}
		sent += count
	}
	for i := 0; i < pipeline; i++ {
		if _, err := readReply(r, 0); err != nil {
			return err
		}
	}
	return nil
}

// A mixed index keeps a deterministic workload without coupling key selection
// to sequence%100, which decides read/write type.
func keyIndex(sequence uint64, keys int) int {
	x := sequence + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return int((x ^ (x >> 31)) % uint64(keys))
}

func cohortPrefix(prefix string, second int64) string {
	return prefix + "cohort:" + strconv.FormatInt(second%4, 10) + ":"
}

func requestKey(o options, sequence int, due time.Time) string {
	prefix := o.Prefix
	if o.CohortExpiry {
		prefix = cohortPrefix(prefix, due.Unix())
	}
	return prefix + strconv.Itoa(keyIndex(uint64(sequence), o.Keys))
}

func preload(o options) error {
	c, err := net.DialTimeout("tcp", o.Address, o.Timeout)
	if err != nil {
		return err
	}
	defer c.Close()
	r := bufio.NewReader(c)
	value := strings.Repeat("x", o.Size)
	if o.Collection != "" {
		for i := 0; i < o.Keys; i++ {
			var request []byte
			switch o.Collection {
			case "hash":
				request = wire("HSET", o.Prefix, "field:"+strconv.Itoa(i), value)
			case "list":
				request = wire("RPUSH", o.Prefix, value)
			case "set":
				request = wire("SADD", o.Prefix, strconv.Itoa(i)+":"+value)
			case "zset":
				request = wire("ZADD", o.Prefix, strconv.Itoa(i), strconv.Itoa(i)+":"+value)
			}
			if err := exchange(c, r, request, o.Timeout); err != nil {
				return err
			}
		}
		return nil
	}
	prefixes := []string{o.Prefix}
	if o.CohortExpiry {
		prefixes = nil
		for cohort := int64(0); cohort < 4; cohort++ {
			prefixes = append(prefixes, cohortPrefix(o.Prefix, cohort))
		}
	}
	for _, prefix := range prefixes {
		for i := 0; i < o.Keys; i++ {
			if err := exchange(c, r, wire("SET", prefix+strconv.Itoa(i), value), o.Timeout); err != nil {
				return err
			}
		}
	}
	return nil
}

type arrival struct {
	due      time.Time
	sequence int
}
type workerResult struct {
	issued, completed, failed, expired uint64
	latency, service, queued           histogram
	firstError                         string
}

func measure(o options) (map[string]any, error) {
	o.Pipeline = max(o.Pipeline, 1)
	started, err := scheduledStart(o.StartNS, time.Now())
	if err != nil {
		return nil, err
	}
	connections := make([]net.Conn, 0, o.Connections)
	defer func() {
		for _, c := range connections {
			c.Close()
		}
	}()
	for i := 0; i < o.Connections; i++ {
		c, err := net.DialTimeout("tcp", o.Address, o.Timeout)
		if err != nil {
			return nil, err
		}
		connections = append(connections, c)
	}
	jobs := make(chan arrival, o.Queue)
	results := make(chan *workerResult, o.Connections)
	var workers sync.WaitGroup
	value := strings.Repeat("x", o.Size)
	for _, c := range connections {
		workers.Add(1)
		go func(c net.Conn) {
			defer workers.Done()
			r := bufio.NewReaderSize(c, 16<<10)
			var result workerResult
			for job := range jobs {
				began := time.Now()
				result.queued.add(began.Sub(job.due))
				if began.Sub(job.due) > o.Timeout {
					result.expired++
					continue
				}
				var request []byte
				if o.Collection == "hash" {
					request = wire("HGETALL", o.Prefix)
				} else if o.Collection == "list" {
					request = wire("LRANGE", o.Prefix, "0", "-1")
				} else if o.Collection == "set" {
					request = wire("SMEMBERS", o.Prefix)
				} else if o.Collection == "zset" {
					request = wire("ZRANGE", o.Prefix, "0", "-1", "WITHSCORES")
				} else {
					// Deterministic mixed key order, identical for each comparison arm.
					key := requestKey(o, job.sequence, job.due)
					if job.sequence%100 < o.Writes {
						if o.CohortExpiry {
							at := job.due.Truncate(time.Second).Add(2 * time.Second).UnixMilli()
							request = wire("SET", key, value, "PXAT", strconv.FormatInt(at, 10))
						} else {
							request = wire("SET", key, value)
						}
					} else {
						request = wire("GET", key)
					}
				}
				result.issued++
				err := exchangePipeline(c, r, request, o.Timeout, o.Pipeline)
				ended := time.Now()
				result.service.add(ended.Sub(began))
				result.latency.add(ended.Sub(job.due))
				if err != nil {
					result.failed++
					if result.firstError == "" {
						result.firstError = err.Error()
					}
					// A failed frame poisons the stream. Do not interpret later bytes
					// as another request's response. Other workers remain available.
					c.Close()
					break
				}
				result.completed++
			}
			results <- &result
		}(c)
	}
	preparedLead := time.Until(started)
	if o.StartNS > 0 && preparedLead <= 0 {
		close(jobs)
		workers.Wait()
		return nil, errors.New("scheduled start passed during connection/worker preparation")
	}
	if o.StartNS == 0 {
		started = time.Now()
	}
	if remaining := time.Until(started); remaining > 0 {
		time.Sleep(remaining)
	}
	var cpuBefore, cpuAfter syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuBefore)
	total := int(float64(o.Rate) * o.Seconds)
	var schedule histogram
	dropped := uint64(0)
	for i := 0; i < total; i++ {
		due := started.Add(time.Duration(int64(i) * int64(time.Second) / int64(o.Rate)))
		if remaining := time.Until(due); remaining > 0 {
			time.Sleep(remaining)
		}
		schedule.add(time.Since(due))
		select {
		case jobs <- arrival{due, i}:
		default:
			dropped++
		}
	}
	close(jobs)
	workers.Wait()
	close(results)
	elapsed := time.Since(started)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &cpuAfter)
	var combined workerResult
	for range jobs {
		combined.expired++
	}
	for r := range results {
		combined.issued += r.issued
		combined.completed += r.completed
		combined.failed += r.failed
		combined.expired += r.expired
		combined.latency.merge(&r.latency)
		combined.service.merge(&r.service)
		combined.queued.merge(&r.queued)
		if combined.firstError == "" {
			combined.firstError = r.firstError
		}
	}
	if combined.completed+combined.failed+combined.expired+dropped != uint64(total) {
		return nil, errors.New("arrival accounting mismatch")
	}
	return map[string]any{
		"options": o, "scheduled": total, "issued": combined.issued, "completed": combined.completed,
		"failed": combined.failed, "queue_dropped": dropped, "queue_expired": combined.expired,
		"completed_commands":       combined.completed * uint64(o.Pipeline),
		"request_unit":             "one pipeline batch; latency includes all replies; failed batches may execute a partial command prefix",
		"prepared_before_start_ms": float64(preparedLead) / float64(time.Millisecond),
		"elapsed_seconds":          elapsed.Seconds(), "completed_per_requested_second": float64(combined.completed) / o.Seconds,
		"completed_per_elapsed_second": float64(combined.completed) / elapsed.Seconds(),
		"scheduled_latency":            combined.latency.summary(), "service_latency": combined.service.summary(),
		"queue_latency": combined.queued.summary(), "scheduler_lag": schedule.summary(), "first_error": combined.firstError,
		"histogram_relative_bucket_width_max": 1.0 / 64,
		"generator_user_seconds":              float64(cpuAfter.Utime.Nano()-cpuBefore.Utime.Nano()) / 1e9,
		"generator_system_seconds":            float64(cpuAfter.Stime.Nano()-cpuBefore.Stime.Nano()) / 1e9,
		"limitations":                         "Scheduled latency includes generator/queue delay. Queue drops are generator admission drops, not server rejections. Generator CPU and scheduler lag must be assessed before attributing capacity to the server.",
	}, nil
}

func scheduledStart(startNS int64, now time.Time) (time.Time, error) {
	if startNS == 0 {
		return now, nil
	}
	started := now.Add(time.Unix(0, startNS).Sub(now))
	if !started.After(now) {
		return time.Time{}, errors.New("scheduled start already passed")
	}
	return started, nil
}

func main() {
	var o options
	var prepare bool
	flag.StringVar(&o.Address, "address", "127.0.0.1:6379", "owned test server address")
	flag.IntVar(&o.Rate, "rate", 10000, "scheduled requests per second")
	flag.Float64Var(&o.Seconds, "seconds", 10, "scheduled workload duration")
	flag.IntVar(&o.Connections, "connections", 32, "independent client connections")
	flag.IntVar(&o.Pipeline, "pipeline", 1, "commands per scheduled batch; all replies finish before batch completion")
	flag.IntVar(&o.Queue, "queue", 64, "bounded pending arrival slots")
	flag.IntVar(&o.Keys, "keys", 10000, "working-set keys or members; hash/zset max 524288, other max 1000000")
	flag.IntVar(&o.Size, "size", 64, "value size in bytes")
	flag.IntVar(&o.Writes, "writes", 5, "SET percentage")
	flag.StringVar(&o.Prefix, "prefix", "arrival:", "owned dataset namespace")
	flag.BoolVar(&o.CohortExpiry, "cohort-expiry", false, "group written key expiry at whole-second boundaries")
	flag.StringVar(&o.Collection, "collection", "", "hash, list, set or zset whole-collection reads")
	flag.DurationVar(&o.Timeout, "timeout", 3*time.Second, "request and queue age ceiling")
	flag.Int64Var(&o.StartNS, "start-ns", 0, "optional common scheduled start for independent tenant generators")
	flag.BoolVar(&prepare, "preload", false, "populate then exit before measurement")
	flag.Parse()
	if o.Pipeline < 1 || o.Pipeline > 4096 || o.Rate <= 0 || o.Rate > 10000000 || !(o.Seconds > 0 && o.Seconds <= 300) || o.Connections < 1 || o.Connections > 4096 || o.Queue < 0 || o.Queue > 65536 || o.Keys < 1 || o.Keys > maxCollectionKeys(o.Collection) || o.Size < 0 || o.Size > 1<<20 || o.Writes < 0 || o.Writes > 100 || o.Timeout <= 0 || o.Timeout > time.Minute || (o.Collection != "" && o.Collection != "hash" && o.Collection != "list" && o.Collection != "set" && o.Collection != "zset") {
		fmt.Fprintln(os.Stderr, "invalid workload bounds")
		os.Exit(2)
	}
	if prepare {
		if err := preload(o); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	report, err := measure(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if report["failed"].(uint64) > 0 {
		os.Exit(1)
	}
}
