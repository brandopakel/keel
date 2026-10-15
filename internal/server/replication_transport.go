package server

import (
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/brandopakel/keel/internal/core"
)

// replicaLink is a replica's transport's view of its link to its primary,
// which INFO's Replication section reports in Redis's terms
// (core.ReplicationLink). The transport's goroutine writes it and the event
// loop reads it, so a lock guards it. Its counts follow Redis's replication.c:
// every connection attempted counts (connectWithMaster), and the current
// count starts again when an established link is lost.
type replicaLink struct {
	mu   sync.Mutex
	link core.ReplicationLink
}

// dialing counts a connection to the primary about to be attempted.
func (l *replicaLink) dialing() {
	l.mu.Lock()
	l.link.Attempts++
	l.link.TotalAttempts++
	l.mu.Unlock()
}

// pulled records a pull that succeeded at now: the link is up, and its last
// IO now. A link coming back up ends an outage, whose time is added to the
// time disconnected.
func (l *replicaLink) pulled(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.link.Up {
		if !l.link.DownSince.IsZero() {
			l.link.Disconnected += now.Sub(l.link.DownSince)
		}
		l.link.Up, l.link.UpSince = true, now
	}
	l.link.LastIO = now
}

// lost records the connection to the primary ending at now: a link that was
// up is down from now, and the current attempts start again.
func (l *replicaLink) lost(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.link.Up {
		l.link.Up, l.link.DownSince, l.link.Attempts = false, now, 0
	}
}

// state is the link as INFO reports it.
func (l *replicaLink) state() core.ReplicationLink {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.link
}

// connectedReplicas are the replicas connected to this server, the
// connections whose pull the engine accepted (respond), in the order they
// connected, as Redis lists its replicas. The event loop calls it, which
// alone touches clients.
func connectedReplicas() []core.ConnectedReplica {
	var replicas []*client
	for _, c := range clients {
		if c.replica {
			replicas = append(replicas, c)
		}
	}
	sort.Slice(replicas, func(i, j int) bool { return replicas[i].id < replicas[j].id })
	out := make([]core.ConnectedReplica, 0, len(replicas))
	for _, c := range replicas {
		host, portText, _ := net.SplitHostPort(c.addr)
		port, _ := strconv.Atoi(portText)
		out = append(out, core.ConnectedReplica{IP: host, Port: port, Offset: c.replicaOffset, Pulled: c.replicaPulled})
	}
	return out
}
