package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"memkv/internal/config"
	"memkv/internal/server"
)

// mode selects the I/O implementation, so the alternatives discussed in the
// upstream performance issue can be benchmarked against each other from one
// binary:
//
//	kqueue-wbuf  event loop, replies coalesced per read      (default)
//	kqueue       event loop, one write syscall per reply      (upstream's design)
//	net          net.Listener with one goroutine per connection
//
// The default coalesces. One write per command is a throughput ceiling rather
// than a trade-off, so serving unbuffered has to be asked for explicitly with
// -mode kqueue. That mode name is also the label the bench results use for
// upstream's design, so it keeps its meaning here.
var mode string

func init() {
	flag.StringVar(&config.Host, "host", "0.0.0.0", "host")
	flag.IntVar(&config.Port, "port", config.Port, "port")
	flag.StringVar(&mode, "mode", "kqueue-wbuf", "io mode: kqueue-wbuf (default) | kqueue (unbuffered) | net | net-small | net-direct | net-chan | net-nolock")
	flag.Parse()
}

func main() {
	fmt.Println("starting memkv database ...")
	var signals = make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	var wg sync.WaitGroup
	wg.Add(2)

	switch mode {
	case "kqueue-wbuf":
		go server.RunAsyncTCPServer(&wg)
	case "kqueue":
		server.WriteUnbuffered = true
		go server.RunAsyncTCPServer(&wg)
	case "net":
		server.ActiveNetVariant = server.NetVariantMutex
		go server.RunNetTCPServer(&wg)
	case "net-small":
		server.ActiveNetVariant = server.NetVariantSmallBuf
		go server.RunNetTCPServer(&wg)
	case "net-direct":
		server.ActiveNetVariant = server.NetVariantDirect
		go server.RunNetTCPServer(&wg)
	case "net-chan":
		server.ActiveNetVariant = server.NetVariantChannel
		go server.RunNetTCPServer(&wg)
	case "net-nolock":
		// diagnostic only: PING-safe, races on any command that touches a store
		server.EvalUnlocked = true
		go server.RunNetTCPServer(&wg)
	default:
		log.Fatalf("unknown -mode %q (want kqueue-wbuf, kqueue or net*)", mode)
	}
	go server.WaitForSignal(&wg, signals)

	wg.Wait()
}
