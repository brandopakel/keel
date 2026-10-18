package core

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file is Redis's latency monitor: the events that took
// latency-monitor-threshold milliseconds or more, and LATENCY LATEST,
// HISTORY, RESET, GRAPH and DOCTOR, which report them. Its reports and its
// graph are translated from Redis 7.2.4's src/latency.c and src/sparkline.c,
// under the BSD 3-Clause licence (THIRD_PARTY_NOTICES.md); Redis 8.10.1's
// are the same but for the address DOCTOR gives when the monitor is off.
//
// It sorts after the command handlers, so that its code moves none of them.

// latencySeriesLen is Redis's LATENCY_TS_LEN: the samples an event keeps,
// one a second at most.
const latencySeriesLen = 160

// latencySample is one second's worst latency of an event, in milliseconds,
// and the Unix second it was in.
type latencySample struct {
	time, latency int64
}

// latencySeries is an event's samples, a ring whose next place is idx, and
// the worst latency it has had since it was last reset: Redis's
// latencyTimeSeries.
type latencySeries struct {
	idx     int
	max     int64
	samples [latencySeriesLen]latencySample
}

// latencyMonitor is the latency monitor's setting and what it has recorded.
type latencyMonitor struct {
	// threshold is latency-monitor-threshold, in milliseconds; 0, its
	// default, turns the monitor off.
	threshold int64
	events    map[string]*latencySeries
}

// add records that event took ms milliseconds in the Unix second now, as
// Redis's latencyAddSample does: a second already sampled keeps the worse of
// the two.
func (m *latencyMonitor) add(event string, ms, now int64) {
	if m.events == nil {
		m.events = make(map[string]*latencySeries)
	}
	ts := m.events[event]
	if ts == nil {
		ts = &latencySeries{}
		m.events[event] = ts
	}
	ts.max = max(ts.max, ms)
	prev := &ts.samples[(ts.idx+latencySeriesLen-1)%latencySeriesLen]
	if prev.time == now {
		prev.latency = max(prev.latency, ms)
		return
	}
	ts.samples[ts.idx] = latencySample{time: now, latency: ms}
	ts.idx = (ts.idx + 1) % latencySeriesLen
}

// reset removes the events named, without regard to case, as Redis's
// latencyResetEvent does, or every event when all; it returns how many it
// removed.
func (m *latencyMonitor) reset(event string, all bool) int64 {
	var resets int64
	for name := range m.events {
		if all || strings.EqualFold(name, event) {
			delete(m.events, name)
			resets++
		}
	}
	return resets
}

// names are the events recorded, in name order. Redis lists them in its
// hash table's order, which no client relies on.
func (m *latencyMonitor) names() []string {
	names := make([]string, 0, len(m.events))
	for name := range m.events {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// noteLatency records that event took ms milliseconds, if that is the
// latency monitor's threshold or more: Redis's latencyAddSampleIfNeeded.
func (e *Engine) noteLatency(event string, ms int64) {
	if t := e.latencyMonitor.threshold; t != 0 && ms >= t {
		e.latencyMonitor.add(event, ms, time.Now().Unix())
	}
}

// noteLong looks at a command that took longFrom microseconds or more: the
// latency monitor records it as Redis's call() does, as a fast-command if
// its spec carries FAST and a command if not, and the slow log takes it if
// it took long enough for that.
func (e *Engine) noteLong(cmd *Command, stat uint16, micros uint64) {
	if t := e.latencyMonitor.threshold; t != 0 && int64(micros/1000) >= t {
		event := "command"
		if spec, ok := commandSpecOf(strings.ToUpper(statNames[stat])); ok && spec.hasFlag("FAST") {
			event = "fast-command"
		}
		e.latencyMonitor.add(event, int64(micros/1000), time.Now().Unix())
	}
	e.noteSlow(cmd, stat, micros)
}

// watchLongCommands sets longFrom, the least time in microseconds a command
// must take for noteLong to look at it: the slow log's bound or the latency
// monitor's, whichever is lower, or never while neither is on. It is called
// whenever either changes.
func (e *Engine) watchLongCommands() {
	from := int64(math.MaxInt64)
	if e.slowlog.slowerThan >= 0 {
		from = e.slowlog.slowerThan
	}
	if t := e.latencyMonitor.threshold; t != 0 && t <= math.MaxInt64/1000 {
		from = min(from, t*1000)
	}
	e.longFrom = from
}

// noteAOFWrite records the log's last write, as Redis's flushAppendOnlyFile
// records its write(): as aof-write, and as aof-write-pending-fsync while a
// sync of the log is under way, aof-write-active-child while a rewrite is, or
// aof-write-alone. Keel's rewrite has no child process; it runs in slices on
// the loop and a worker, and is what competes with the write for the disk as
// Redis's child does.
func (e *Engine) noteAOFWrite() {
	if e.latencyMonitor.threshold == 0 {
		return
	}
	ms := int64(e.appendWriteStats.lastNS.Load() / uint64(time.Millisecond))
	switch {
	case e.aof.syncPending != nil:
		e.noteLatency("aof-write-pending-fsync", ms)
	case e.rewrite.active:
		e.noteLatency("aof-write-active-child", ms)
	default:
		e.noteLatency("aof-write-alone", ms)
	}
	e.noteLatency("aof-write", ms)
}

// syncAOFInLine syncs the log, timed, before the replies it covers, and under
// appendfsync always the latency monitor records the sync as
// aof-fsync-always, as Redis's does.
func (e *Engine) syncAOFInLine(fsync FsyncPolicy) error {
	err := timedPersistenceSync(&e.appendSyncStats, e.aof.file, e.aofSync)
	if fsync == FsyncAlways && err == nil {
		e.noteAOFSyncAlways()
	}
	return err
}

// noteAOFSyncAlways records the log's last sync, made under appendfsync
// always before the replies it covers, as aof-fsync-always.
func (e *Engine) noteAOFSyncAlways() {
	e.noteLatency("aof-fsync-always", int64(e.appendSyncStats.lastNS.Load()/uint64(time.Millisecond)))
}

// noteAppended records the batch an append worker wrote off the loop
// (FlushAOFAsync), and synced, as the loop collects it (pollAppend): the write as
// noteAOFWrite records the loop's own, and the sync, made under appendfsync
// always, as aof-fsync-always. The timings are the worker's, which it set
// before it handed the batch back; whether a sync or a rewrite was under way
// is what the loop finds as it collects the batch. The replies the batch
// covers waited for it, as Redis's wait for its write, so its time is the
// event's. It is kept out of line, so that the poll that calls it does not
// grow by what it does.
//
//go:noinline
func (e *Engine) noteAppended() {
	if e.latencyMonitor.threshold == 0 {
		return
	}
	e.noteAOFWrite()
	// The worker syncs under always, and a batch it synced has had
	// appendSynced moved to its end, which appendCompleted marks.
	if e.settings.fsync == FsyncAlways && e.appendSynced == e.appendCompleted {
		e.noteAOFSyncAlways()
	}
}

// noteSyncCollected records a sync made off the loop under appendfsync
// always, as an engine Open's is, which publishes only what a sync has
// covered (flushAOF): as aof-fsync-always, as the loop collects it. A sync
// under everysec runs in the background as Redis's does, and is not an event.
// It is kept out of line, as noteAppended is.
//
//go:noinline
func (e *Engine) noteSyncCollected() {
	if e.settings.fsync == FsyncAlways {
		e.noteAOFSyncAlways()
	}
}

// noteExpireCycle adds the time an expiry cycle took, from started to ended
// by nanotime and rounded as commandstats rounds a command's, to the cycles',
// and the latency monitor records it as expire-cycle.
func (e *Engine) noteExpireCycle(started, ended int64) {
	usec := microsBetween(started, ended)
	e.expireCycleUsec += usec
	e.noteLatency("expire-cycle", int64(usec/1000))
}

// latencyMonitorReply answers LATENCY LATEST, HISTORY, RESET, GRAPH and
// DOCTOR, whose counts have been checked, as Redis's latencyCommand does.
func (e *Engine) latencyMonitorReply(args []string) interface{} {
	m := &e.latencyMonitor
	switch strings.ToLower(args[0]) {
	case "latest":
		latest := []interface{}{}
		for _, name := range m.names() {
			ts := m.events[name]
			last := ts.samples[(ts.idx+latencySeriesLen-1)%latencySeriesLen]
			latest = append(latest, []interface{}{name, last.time, last.latency, ts.max})
		}
		return latest
	case "history":
		history := []interface{}{}
		if ts := m.events[args[1]]; ts != nil {
			for _, s := range ts.inOrder() {
				history = append(history, []interface{}{s.time, s.latency})
			}
		}
		return history
	case "reset":
		if len(args) == 1 {
			return m.reset("", true)
		}
		var resets int64
		for _, event := range args[1:] {
			resets += m.reset(event, false)
		}
		return resets
	case "graph":
		ts := m.events[args[1]]
		if ts == nil {
			return fmt.Errorf("ERR No samples available for event '%s'", args[1])
		}
		return ReplyVerbatim(latencyGraph(args[1], ts, time.Now().Unix()))
	}
	return ReplyVerbatim(e.latencyReport(time.Now().Unix(), anonHugePagesBytes()))
}

// inOrder are the samples ts holds, oldest first.
func (ts *latencySeries) inOrder() []latencySample {
	var samples []latencySample
	for j := 0; j < latencySeriesLen; j++ {
		if s := ts.samples[(ts.idx+j)%latencySeriesLen]; s.time != 0 {
			samples = append(samples, s)
		}
	}
	return samples
}

// latencyStats is Redis's latencyStats for an event: the average and mean
// absolute deviation of its samples, and the seconds from its oldest to now,
// at least 1.
type latencyStats struct {
	avg, mad, samples, period int64
}

// analyze is Redis's analyzeLatencyForEvent: integer averages, as Redis
// computes them.
func (ts *latencySeries) analyze(now int64) latencyStats {
	samples := ts.inOrder()
	var ls latencyStats
	if len(samples) == 0 {
		return ls
	}
	var sum, oldest int64
	for _, s := range samples {
		sum += s.latency
		if oldest == 0 || s.time < oldest {
			oldest = s.time
		}
	}
	ls.samples = int64(len(samples))
	ls.avg = sum / ls.samples
	ls.period = max(now-oldest, 1)
	var deviation int64
	for _, s := range samples {
		d := ls.avg - s.latency
		if d < 0 {
			d = -d
		}
		deviation += d
	}
	ls.mad = deviation / ls.samples
	return ls
}

// The advice LATENCY DOCTOR gives, in the order it gives it, as Redis's
// createLatencyReport writes it.
const (
	adviseSlowlogEnabled = iota
	adviseSlowlogTuning
	adviseSlowlogInspect
	adviseScheduler
	adviseLocalDisk
	adviseSSD
	adviseDataWriteback
	adviseDiskContention
	adviseNoAppendfsync
	adviseRelaxFsyncPolicy
	adviseWriteLoadInfo
	adviseHz
	adviseLargeObjects
	adviseMassEviction
	adviseDisableTHP
	adviceKinds
)

// latencyReport is LATENCY DOCTOR's report at the Unix second now, given
// the bytes of anonymous huge pages the process has, as Redis's
// createLatencyReport writes it. Redis's advice on fork, which Keel never
// does, is left out, and its advice on hz where there is no server to set it
// on.
func (e *Engine) latencyReport(now int64, hugePages int64) string {
	m := &e.latencyMonitor
	if len(m.events) == 0 && m.threshold == 0 {
		return latencyDoctorDisabled
	}
	var b strings.Builder
	var advise [adviceKinds]bool
	advices := 0
	set := func(kinds ...int) {
		for _, k := range kinds {
			advise[k] = true
		}
		advices += len(kinds)
	}
	names := m.names()
	for i, event := range names {
		ts := m.events[event]
		if i == 0 {
			b.WriteString("Dave, I have observed latency spikes in this Redis instance. You don't mind talking about it, do you Dave?\n\n")
		}
		ls := ts.analyze(now)
		fmt.Fprintf(&b, "%d. %s: %d latency spikes (average %dms, mean deviation %dms, period %.2f sec). Worst all time event %dms.",
			i+1, event, ls.samples, ls.avg, ls.mad, float64(ls.period)/float64(ls.samples), ts.max)
		switch strings.ToLower(event) {
		case "command":
			switch {
			case e.slowlog.slowerThan < 0 || e.slowlog.maxLen == 0:
				set(adviseSlowlogEnabled)
			case e.slowlog.slowerThan/1000 > m.threshold:
				set(adviseSlowlogTuning)
			}
			set(adviseSlowlogInspect, adviseLargeObjects)
		case "fast-command":
			set(adviseScheduler)
		case "aof-write-pending-fsync":
			set(adviseLocalDisk, adviseDiskContention, adviseSSD, adviseDataWriteback)
		case "aof-write-active-child":
			set(adviseNoAppendfsync, adviseDataWriteback, adviseSSD)
		case "aof-write-alone":
			set(adviseLocalDisk, adviseDataWriteback, adviseSSD)
		case "aof-fsync-always":
			set(adviseRelaxFsyncPolicy)
		case "aof-fstat", "rdb-unlink-temp-file":
			set(adviseDiskContention, adviseLocalDisk)
		case "aof-rewrite-diff-write", "aof-rename":
			set(adviseWriteLoadInfo, adviseDataWriteback, adviseSSD, adviseLocalDisk)
		case "expire-cycle":
			set(adviseHz, adviseLargeObjects)
		case "eviction-del":
			set(adviseLargeObjects)
		case "eviction-cycle":
			set(adviseMassEviction)
		}
		b.WriteString("\n")
	}
	if hugePages > 0 {
		set(adviseDisableTHP)
	}

	switch {
	case len(names) == 0 && advices == 0:
		b.WriteString("Dave, no latency spike was observed during the lifetime of this Redis instance, not in the slightest bit. I honestly think you ought to sit down calmly, take a stress pill, and think things over.\n")
		return b.String()
	case len(names) > 0 && advices == 0:
		b.WriteString("\nWhile there are latency events logged, I'm not able to suggest any easy fix. Please use the Redis community to get some help, providing this report in your help request.\n")
		return b.String()
	}
	b.WriteString("\nI have a few advices for you:\n\n")
	threshold := strconv.FormatUint(uint64(m.threshold)*1000, 10)
	for kind, text := range latencyAdvice {
		switch {
		case !advise[kind]:
		case kind == adviseRelaxFsyncPolicy && e.settings.fsync != FsyncAlways:
		case kind == adviseHz && (e.serverInfo == nil || e.serverInfo.Hz >= 100):
			// An engine with no server has no hz to set.
		case kind == adviseSlowlogEnabled || kind == adviseSlowlogTuning:
			b.WriteString(strings.Replace(text, "%llu", threshold, 1))
		default:
			b.WriteString(text)
		}
	}
	return b.String()
}

// latencyAdvice is each piece of LATENCY DOCTOR's advice, as Redis writes
// it; %llu stands for the threshold in microseconds.
var latencyAdvice = [adviceKinds]string{
	adviseSlowlogEnabled: "- There are latency issues with potentially slow commands you are using. Try to enable the Slow Log Redis feature using the command 'CONFIG SET slowlog-log-slower-than %llu'. If the Slow log is disabled Redis is not able to log slow commands execution for you.\n",
	adviseSlowlogTuning:  "- Your current Slow Log configuration only logs events that are slower than your configured latency monitor threshold. Please use 'CONFIG SET slowlog-log-slower-than %llu'.\n",
	adviseSlowlogInspect: "- Check your Slow Log to understand what are the commands you are running which are too slow to execute. Please check https://redis.io/commands/slowlog for more information.\n",
	adviseScheduler: "- The system is slow to execute Redis code paths not containing system calls. This usually means the system does not provide Redis CPU time to run for long periods. You should try to:\n" +
		"  1) Lower the system load.\n" +
		"  2) Use a computer / VM just for Redis if you are running other software in the same system.\n" +
		"  3) Check if you have a \"noisy neighbour\" problem.\n" +
		"  4) Check with 'redis-cli --intrinsic-latency 100' what is the intrinsic latency in your system.\n" +
		"  5) Check if the problem is allocator-related by recompiling Redis with MALLOC=libc, if you are using Jemalloc. However this may create fragmentation problems.\n",
	adviseLocalDisk:        "- It is strongly advised to use local disks for persistence, especially if you are using AOF. Remote disks provided by platform-as-a-service providers are known to be slow.\n",
	adviseSSD:              "- SSD disks are able to reduce fsync latency, and total time needed for snapshotting and AOF log rewriting (resulting in smaller memory usage). With extremely high write load SSD disks can be a good option. However Redis should perform reasonably with high load using normal disks. Use this advice as a last resort.\n",
	adviseDataWriteback:    "- Mounting ext3/4 filesystems with data=writeback can provide a performance boost compared to data=ordered, however this mode of operation provides less guarantees, and sometimes it can happen that after a hard crash the AOF file will have a half-written command at the end and will require to be repaired before Redis restarts.\n",
	adviseDiskContention:   "- Try to lower the disk contention. This is often caused by other disk intensive processes running in the same computer (including other Redis instances).\n",
	adviseNoAppendfsync:    "- Assuming from the point of view of data safety this is viable in your environment, you could try to enable the 'no-appendfsync-on-rewrite' option, so that fsync will not be performed while there is a child rewriting the AOF file or producing an RDB file (the moment where there is high disk contention).\n",
	adviseRelaxFsyncPolicy: "- Your fsync policy is set to 'always'. It is very hard to get good performances with such a setup, if possible try to relax the fsync policy to 'onesec'.\n",
	adviseWriteLoadInfo:    "- Latency during the AOF atomic rename operation or when the final difference is flushed to the AOF file at the end of the rewrite, sometimes is caused by very high write load, causing the AOF buffer to get very large. If possible try to send less commands to accomplish the same work, or use Lua scripts to group multiple operations into a single EVALSHA call.\n",
	adviseHz:               "- In order to make the Redis keys expiring process more incremental, try to set the 'hz' configuration parameter to 100 using 'CONFIG SET hz 100'.\n",
	adviseLargeObjects:     "- Deleting, expiring or evicting (because of maxmemory policy) large objects is a blocking operation. If you have very large objects that are often deleted, expired, or evicted, try to fragment those objects into multiple smaller objects.\n",
	adviseMassEviction:     "- Sudden changes to the 'maxmemory' setting via 'CONFIG SET', or allocation of large objects via sets or sorted sets intersections, STORE option of SORT, Redis Cluster large keys migrations (RESTORE command), may create sudden memory pressure forcing the server to block trying to evict keys. \n",
	adviseDisableTHP:       "- I detected a non zero amount of anonymous huge pages used by your process. This creates very serious latency events in different conditions, especially when Redis is persisting on disk. To disable THP support use the command 'echo never > /sys/kernel/mm/transparent_hugepage/enabled', make sure to also add it into /etc/rc.local so that the command will be executed again after a reboot. Note that even if you have already disabled THP, you still need to restart the Redis process to get rid of the huge pages already created.\n",
}

// latencyDoctorDisabled is LATENCY DOCTOR's report while the monitor is off
// and has recorded nothing, with Redis 8.10.1's address.
const latencyDoctorDisabled = "I'm sorry, Dave, I can't do that. Latency monitoring is disabled in this Redis instance. " +
	"You may use \"CONFIG SET latency-monitor-threshold <milliseconds>.\" in order to enable it. If we weren't in a deep " +
	"space mission I'd suggest to take a look at " +
	"https://redis.io/docs/latest/operate/oss_and_stack/management/optimization/latency-monitor.\n"

// anonHugePagesBytes is the anonymous huge pages the process has, from
// /proc/self/smaps, as Redis's THPGetAnonHugePagesSize reads them; 0 where
// there is no such file.
func anonHugePagesBytes() int64 {
	data, err := os.ReadFile("/proc/self/smaps")
	if err != nil {
		return 0
	}
	const field = "AnonHugePages:"
	var total int64
	for text := string(data); ; {
		i := strings.Index(text, field)
		if i < 0 {
			return total
		}
		text = text[i+len(field):]
		line := text
		if end := strings.IndexByte(text, '\n'); end >= 0 {
			line = text[:end]
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			kb, _ := strconv.ParseInt(fields[0], 10, 64)
			total += kb * 1024
		}
	}
}

// latencyGraphColumns is Redis's LATENCY_GRAPH_COLS.
const latencyGraphColumns = 80

// latencyGraph is LATENCY GRAPH's graph of event's samples at the Unix second
// now, as Redis's latencyCommandGenSparkeline draws it: a heading, a rule,
// and a filled sparkline four rows high, each sample labelled with how long
// ago it was.
func latencyGraph(event string, ts *latencySeries, now int64) string {
	var seq sparkline
	var low, high int64
	for i, s := range ts.inOrder() {
		if i == 0 {
			low, high = s.latency, s.latency
		}
		low, high = min(low, s.latency), max(high, s.latency)
		elapsed := now - s.time
		var label string
		switch {
		case elapsed < 60:
			label = fmt.Sprintf("%ds", elapsed)
		case elapsed < 3600:
			label = fmt.Sprintf("%dm", elapsed/60)
		case elapsed < 3600*24:
			label = fmt.Sprintf("%dh", elapsed/3600)
		default:
			label = fmt.Sprintf("%dd", elapsed/(3600*24))
		}
		seq.add(float64(s.latency), label)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s - high %d ms, low %d ms (all time high %d ms)\n", event, high, low, ts.max)
	b.WriteString(strings.Repeat("-", latencyGraphColumns))
	b.WriteString("\n")
	seq.render(&b, latencyGraphColumns, 4)
	return b.String()
}

// sparkline is Redis's sparkline sequence: samples, each with a label, and
// their least and greatest.
type sparkline struct {
	values   []float64
	labels   []string
	labelled int
	min, max float64
}

func (s *sparkline) add(value float64, label string) {
	if len(s.values) == 0 {
		s.min, s.max = value, value
	} else if value < s.min {
		s.min = value
	} else if value > s.max {
		s.max = value
	}
	s.values = append(s.values, value)
	s.labels = append(s.labels, label)
	if label != "" {
		s.labelled++
	}
}

// render draws s, filled, in rows of at most columns samples, as Redis's
// sparklineRender with SPARKLINE_FILL does.
func (s *sparkline) render(b *strings.Builder, columns, rows int) {
	for j := 0; j < len(s.values); j += columns {
		if j != 0 {
			b.WriteString("\n")
		}
		s.renderRange(b, rows, j, min(len(s.values)-j, columns))
	}
}

// renderRange is Redis's sparklineRenderRange: the samples from offset, len
// of them, drawn rows high in the characters _, o and #, a | below the top
// of a column, and under them, after a blank line, each sample's label read
// downwards.
func (s *sparkline) renderRange(b *strings.Builder, rows, offset, length int) {
	const fill = "_o#"
	const labelMarginTop = 1
	relmax := s.max - s.min
	if relmax == 0 {
		relmax = 1
	}
	steps := len(fill) * rows
	chars := make([]byte, length)
	for row, more := 0, true; more; {
		more = false
		for j := range chars {
			chars[j] = ' '
		}
		for j := 0; j < length; j++ {
			value, label := s.values[j+offset], s.labels[j+offset]
			// As Redis's (int)(relval*steps)/relmax, truncated twice.
			step := int(float64(int((value-s.min)*float64(steps))) / relmax)
			step = min(max(step, 0), steps-1)
			if row < rows {
				more = true
				charIdx := step - (rows-row-1)*len(fill)
				if charIdx >= 0 && charIdx < len(fill) {
					chars[j] = fill[charIdx]
				} else if charIdx >= len(fill) {
					chars[j] = '|'
				}
				continue
			}
			if s.labelled > 0 && row-rows < labelMarginTop {
				more = true
				break
			}
			if at := row - rows - labelMarginTop; label != "" && len(label) > at {
				more = true
				chars[j] = label[at]
			}
		}
		if more {
			row++
			b.Write(chars)
			b.WriteString("\n")
		}
	}
}
