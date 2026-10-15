package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestINFOThreadsHotkeysAndModules: Threads gives a line for each I/O thread
// the transport reports, as Redis writes them, and Stats what the threads but
// the loop's made; Hotkeys and Modules are headers alone; and module_list asks
// for Modules, as in Redis (server.c).
func TestINFOThreadsHotkeysAndModules(t *testing.T) {
	t.Parallel()
	e := newTestEngine(t, Options{})
	e.SetClientBuffers(func() ClientBufferStats { return ClientBufferStats{Connected: 2} })
	e.SetIOThreads(func() []IOThreadStats {
		return []IOThreadStats{{Clients: 2, Reads: 5, Writes: 4}, {Reads: 3, Writes: 1}}
	})
	info := runOn(t, e, "INFO").(string)
	assert.Contains(t, info, "# Threads\r\nio_thread_0:clients=2,reads=5,writes=4\r\nio_thread_1:clients=0,reads=3,writes=1\r\n\r\n")
	assert.Contains(t, info, "# Hotkeys\r\n\r\n# Modules\r\n\r\n")
	fields := infoFields(t, runOn(t, e, "INFO", "stats").(string))
	assert.Equal(t, "3", fields["io_threaded_reads_processed"], "the threads' but the loop's, as Redis sums them")
	assert.Equal(t, "1", fields["io_threaded_writes_processed"])

	for _, section := range []string{"modules", "module_list", "MODULES"} {
		assert.Equal(t, []string{"Modules"}, infoSectionNames(runOn(t, e, "INFO", section).(string)), section)
	}
	assert.Equal(t, []string{"Hotkeys"}, infoSectionNames(runOn(t, e, "INFO", "hotkeys").(string)))
	assert.Equal(t, []string{"Threads"}, infoSectionNames(runOn(t, e, "INFO", "threads").(string)))

	core := newTestEngine(t, Options{})
	assert.NotContains(t, runOn(t, core, "INFO").(string), "# Threads", "an engine with no transport has no I/O threads")
}
