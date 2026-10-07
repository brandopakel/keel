package procinfo

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestOSNamesTheSystem(t *testing.T) {
	t.Parallel()
	got := OS()
	want := map[string]string{"linux": "Linux ", "darwin": "Darwin "}[runtime.GOOS]
	if got == "" || (want != "" && !strings.HasPrefix(got, want)) {
		t.Fatalf("OS() = %q, want it to start with %q", got, want)
	}
	if want != "" && len(strings.Fields(got)) != 3 {
		t.Fatalf("OS() = %q, want the name, the release and the machine", got)
	}
}

func TestMemoryIsReportedOnLinuxOnly(t *testing.T) {
	t.Parallel()
	rss, rssOK := ResidentBytes()
	total, totalOK := PhysicalBytes()
	if runtime.GOOS != "linux" {
		if rssOK || totalOK {
			t.Fatalf("ResidentBytes and PhysicalBytes answered on %s, which cannot", runtime.GOOS)
		}
		return
	}
	if !rssOK || !totalOK || rss == 0 || total == 0 || rss > total {
		t.Fatalf("rss %d (%t), physical %d (%t)", rss, rssOK, total, totalOK)
	}
}

func TestCPUTimeGrowsWithWork(t *testing.T) {
	t.Parallel()
	before, ok := CPUTime()
	if !ok {
		t.Skip("no getrusage on " + runtime.GOOS)
	}
	deadline := time.Now().Add(20 * time.Millisecond)
	for x := 0; time.Now().Before(deadline); x++ {
		_ = x * x
	}
	after, _ := CPUTime()
	if after.User+after.Sys <= before.User+before.Sys {
		t.Fatalf("CPU time did not grow: %+v then %+v", before, after)
	}
}

func TestCString(t *testing.T) {
	t.Parallel()
	if got := cString([]int8{'L', 'i', 0, 'x'}); got != "Li" {
		t.Fatalf("cString = %q", got)
	}
	if got := cString([]uint8{'a', 'b'}); got != "ab" {
		t.Fatalf("cString = %q", got)
	}
}
