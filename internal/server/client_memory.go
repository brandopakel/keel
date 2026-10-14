package server

// Client memory in Redis's terms, for INFO: what the connections hold, apart
// for the replicas' and the others', and the most one has held lately.
//
// Redis's clientsCron samples each client about once a second
// (clientsCronTrackExpansiveClients, server.c). Here the INFO hook samples
// every connection when the engine takes its rate sample, ten times a second,
// and when INFO runs, so the per-command path gains nothing. A connection's
// size is what accountClient last found it retained: its request buffer and
// parsed requests, and its reply buffer.

// clientPeakSlots is Redis's CLIENTS_PEAK_MEM_USAGE_SLOTS: one slot a
// second, the most recent seven of which make the recent maximum.
const clientPeakSlots = 8

// clientBufferPeaks is Redis's ClientsPeakMemInput and ClientsPeakMemOutput:
// the most any connection held of request and of reply in each of the last
// seconds, a slot a second, slot second%8.
type clientBufferPeaks struct {
	second       [clientPeakSlots]int64
	input, reply [clientPeakSlots]int
}

// note records sizes seen in second now.
func (p *clientBufferPeaks) note(now int64, input, reply int) {
	slot := now % clientPeakSlots
	if p.second[slot] != now {
		// The slot last held a second eight ago; Redis zeroes it as the
		// second before it begins (clientsCron).
		p.second[slot], p.input[slot], p.reply[slot] = now, 0, 0
	}
	p.input[slot] = max(p.input[slot], input)
	p.reply[slot] = max(p.reply[slot], reply)
}

// recent is client_recent_max_input_buffer and
// client_recent_max_output_buffer in second now: the most of each over the
// slots of the last seven seconds, now's included, which are what Redis's
// slots hold once it has zeroed the one after now's (getExpansiveClientsInfo).
func (p *clientBufferPeaks) recent(now int64) (input, reply int) {
	for slot := range p.second {
		if age := now - p.second[slot]; age >= 0 && age < clientPeakSlots-1 {
			input, reply = max(input, p.input[slot]), max(reply, p.reply[slot])
		}
	}
	return input, reply
}

// clientPeaks is the event loop's, as the connections are.
var clientPeaks clientBufferPeaks

// clientMemory is what the connections hold, as INFO reports it in second
// now: how many are not replicas (Redis's connected_clients counts the
// replicas apart), the bytes the others and the replicas retain
// (mem_clients_normal and mem_clients_slaves), and the recent maxima, which
// it samples the connections into.
type clientMemory struct {
	connected, normal, replicas int
	recentInput, recentReply    int
}

func sampleClientMemory(now int64) clientMemory {
	var m clientMemory
	input, reply := 0, 0
	for _, c := range clients {
		held := c.accountedInput + c.accountedReply
		if c.replica {
			m.replicas += held
		} else {
			m.connected++
			m.normal += held
		}
		// Redis tracks every client, a replica too.
		input, reply = max(input, c.accountedInput), max(reply, c.accountedReply)
	}
	clientPeaks.note(now, input, reply)
	m.recentInput, m.recentReply = clientPeaks.recent(now)
	return m
}
