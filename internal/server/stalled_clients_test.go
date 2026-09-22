package server

import (
	"bytes"
	"log"
	"syscall"
	"testing"
	"time"

	"github.com/brandopakel/keel/internal/core"
	"github.com/brandopakel/keel/internal/core/io_multiplexing"
	"github.com/stretchr/testify/require"
)

// The sweep is the one place a connection the loop has stopped serving is
// noticed. Every state that means "a reply is owed" must be closed and said;
// every state that is the client's own doing must be closed and counted; a
// connection with nothing pending must be left alone however old it is.
func TestSweepClosesEveryStalledStateAndNamesTheUnansweredOnes(t *testing.T) {
	oldClients := clients
	clients = make(map[int]*client)
	t.Cleanup(func() { clients = oldClients })
	oldSlow, oldUnanswered := clientsClosedSlow, clientsClosedUnanswered
	t.Cleanup(func() { clientsClosedSlow, clientsClosedUnanswered = oldSlow, oldUnanswered })
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	stale := time.Now().Add(-stalledClientTimeout - time.Second)
	fresh := time.Now()
	parsed := []*core.Command{{Cmd: "GET", Args: []string{"k"}}}
	cases := []struct {
		name   string
		c      *client
		closed bool
		said   bool
	}{
		{"idle forever", &client{lastProgress: stale}, false, false},
		{"slow reader", &client{lastProgress: stale, out: []byte("+OK\r\n")}, true, false},
		{"half-sent request", &client{lastProgress: stale, buf: &connBuffer{}}, true, false},
		{"parsed and never executed", &client{lastProgress: stale, cmds: parsed}, true, true},
		{"held reply", &client{lastProgress: stale, out: []byte("+OK\r\n"), appendHeld: true}, true, true},
		{"deferred run", &client{lastProgress: stale, cmds: parsed, appendDeferred: true}, true, true},
		{"queued continuation", &client{lastProgress: stale, cmds: parsed, readQueued: true}, true, true},
		{"recent parsed run", &client{lastProgress: fresh, cmds: parsed}, false, false},
		{"recent slow reader", &client{lastProgress: fresh, out: []byte("+OK\r\n")}, false, false},
	}
	for i := range cases {
		r, _ := socketPair(t)
		cases[i].c.fd = r
		cases[i].c.interest, cases[i].c.interestKnown = io_multiplexing.OpNone, true
		clients[r] = cases[i].c
	}
	forgotten := map[int]bool{}
	mux := &forgettingMonitor{registered: map[int]bool{cases[3].c.fd: true}}
	closed := sweepStalledClients(time.Now(), mux, nil, func(c *client) { forgotten[c.fd] = true })

	wantClosed, wantSaid := 0, 0
	for _, tc := range cases {
		_, live := clients[tc.c.fd]
		require.Equal(t, !tc.closed, live, tc.name)
		require.Equal(t, tc.closed, forgotten[tc.c.fd], tc.name)
		if tc.closed {
			wantClosed++
		}
		if tc.said {
			wantSaid++
		}
	}
	require.Equal(t, wantClosed, closed)
	require.Equal(t, oldSlow+uint64(wantClosed-wantSaid), clientsClosedSlow)
	require.Equal(t, oldUnanswered+uint64(wantSaid), clientsClosedUnanswered)
	require.Equal(t, wantSaid, bytes.Count(logged.Bytes(), []byte("request unanswered")),
		"every unanswered request is logged, nothing else is:\n%s", logged.String())
	require.Contains(t, logged.String(), "parsed=1 reply=0 partial=false held=false deferred=false queued=false interest=2 known=true registered=true")
	require.Contains(t, logged.String(), "held=true")
	require.Contains(t, logged.String(), "registered=false")
	require.Equal(t, wantSaid, len(mux.forgotten), "the kernel is asked about every unanswered connection and no other")
}

// forgettingMonitor answers Forget from a table and records who was asked.
type forgettingMonitor struct {
	io_multiplexing.IOMultiplexer
	registered map[int]bool
	forgotten  []int
}

func (m *forgettingMonitor) Forget(fd int) bool {
	m.forgotten = append(m.forgotten, fd)
	return m.registered[fd]
}

func TestUnansweredCoversEveryOwedState(t *testing.T) {
	require.False(t, (&client{}).unanswered())
	require.False(t, (&client{out: []byte("x")}).unanswered(), "a produced reply the client has not read is not owed by the server")
	require.False(t, (&client{buf: &connBuffer{}}).unanswered(), "a half-sent request has not been accepted yet")
	require.True(t, (&client{cmds: []*core.Command{{Cmd: "PING"}}}).unanswered())
	require.True(t, (&client{appendHeld: true}).unanswered())
	require.True(t, (&client{appendDeferred: true}).unanswered())
	require.True(t, (&client{readQueued: true}).unanswered())
}

// The sweep's first version could not see the fault it was written for. On
// September 22, 2026 a primary stopped serving an established connection, and
// the sweep waited its full thirty seconds and closed nothing: the loop had
// never read the request, so it owed nothing, held nothing, and every check
// passed the connection over while its bytes sat in the socket.
func TestSweepSeesARequestTheLoopNeverRead(t *testing.T) {
	oldClients := clients
	clients = make(map[int]*client)
	t.Cleanup(func() { clients = oldClients })
	oldUnread := clientsClosedUnread
	t.Cleanup(func() { clientsClosedUnread = oldUnread })
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	stale := time.Now().Add(-stalledClientTimeout - time.Second)
	request := []byte("*1\r\n$4\r\nPING\r\n")
	unread, peer := socketPair(t)
	clients[unread] = &client{fd: unread, lastProgress: stale,
		interest: io_multiplexing.OpRead, interestKnown: true}
	if _, err := syscall.Write(peer, request); err != nil {
		t.Fatal(err)
	}
	// A peer that has simply gone quiet has sent nothing, and is left alone
	// however long it has been silent.
	quiet, _ := socketPair(t)
	clients[quiet] = &client{fd: quiet, lastProgress: stale,
		interest: io_multiplexing.OpRead, interestKnown: true}

	mux := &forgettingMonitor{registered: map[int]bool{}}
	now := time.Now()
	require.Equal(t, 0, sweepStalledClients(now, mux, nil, nil),
		"first sight is not evidence: the request may have landed after the loop's last wait")
	require.Equal(t, 0, sweepStalledClients(now.Add(500*time.Millisecond), mux, nil, nil),
		"nor is a second look within the second")
	require.Equal(t, 1, sweepStalledClients(now.Add(time.Second), mux, nil, nil))
	require.NotContains(t, clients, unread, "the connection holding an unread request is closed")
	require.Contains(t, clients, quiet, "a quiet connection is not")
	require.Equal(t, oldUnread+1, clientsClosedUnread)
	require.Equal(t, []int{unread}, mux.forgotten, "only the unread connection is asked about")
	line := logged.String()
	require.Contains(t, line, "14 bytes unread for at least 1s")
	require.Contains(t, line, "with nothing pending")
	require.Contains(t, line, "registered=false")
}

// Bytes on a long-idle connection are usually a request that arrived a moment
// ago, not a lost registration. A pooled connection that speaks after a long
// silence, just as the sweep runs, must be read and answered, not closed; and
// a connection the loop stopped reading on purpose, while a flush completes,
// is holding its request exactly as intended.
func TestSweepLeavesARequestTheLoopIsAboutToRead(t *testing.T) {
	oldClients := clients
	clients = make(map[int]*client)
	t.Cleanup(func() { clients = oldClients })
	oldUnread := clientsClosedUnread
	t.Cleanup(func() { clientsClosedUnread = oldUnread })

	stale := time.Now().Add(-stalledClientTimeout - time.Second)
	request := []byte("*1\r\n$4\r\nPING\r\n")
	arriving, arrivingPeer := socketPair(t)
	clients[arriving] = &client{fd: arriving, lastProgress: stale,
		interest: io_multiplexing.OpRead, interestKnown: true}
	held, heldPeer := socketPair(t)
	clients[held] = &client{fd: held, lastProgress: stale,
		interest: io_multiplexing.OpNone, interestKnown: true}
	for _, fd := range []int{arrivingPeer, heldPeer} {
		if _, err := syscall.Write(fd, request); err != nil {
			t.Fatal(err)
		}
	}
	paused := map[int]*client{held: clients[held]}

	mux := &forgettingMonitor{registered: map[int]bool{}}
	now := time.Now()
	require.Equal(t, 0, sweepStalledClients(now, mux, paused, nil))
	// The loop's next wait reports the request and the read is progress.
	clients[arriving].lastProgress = now.Add(10 * time.Millisecond)
	require.Equal(t, 0, sweepStalledClients(now.Add(2*time.Second), mux, paused, nil))
	require.Contains(t, clients, arriving)
	require.Contains(t, clients, held)
	require.Equal(t, oldUnread, clientsClosedUnread)
	require.Empty(t, mux.forgotten)
}
