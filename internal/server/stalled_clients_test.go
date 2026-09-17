package server

import (
	"bytes"
	"log"
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
	closed := sweepStalledClients(time.Now(), func(c *client) { forgotten[c.fd] = true })

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
	require.Contains(t, logged.String(), "parsed=1 reply=0 partial=false held=false deferred=false queued=false interest=2 known=true")
	require.Contains(t, logged.String(), "held=true")
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
