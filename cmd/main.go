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
//	kqueue       event loop, one write syscall per reply   (current design)
//	kqueue-wbuf  event loop, replies coalesced per read
//	net          net.Listener with one goroutine per connection
var mode string

func init() {
	flag.StringVar(&config.Host, "host", "0.0.0.0", "host")
	flag.IntVar(&config.Port, "port", config.Port, "port")
	flag.StringVar(&mode, "mode", "kqueue", "io mode: kqueue | kqueue-wbuf | net | net-nolock (diagnostic)")
	flag.Parse()
}

func main() {
	fmt.Println("starting memkv database ...")
	var signals = make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	var wg sync.WaitGroup
	wg.Add(2)

	switch mode {
	case "kqueue":
		go server.RunAsyncTCPServer(&wg)
	case "kqueue-wbuf":
		server.WriteBuffered = true
		go server.RunAsyncTCPServer(&wg)
	case "net":
		go server.RunNetTCPServer(&wg)
	case "net-nolock":
		// diagnostic only: PING-safe, races on any command that touches a store
		server.EvalUnlocked = true
		go server.RunNetTCPServer(&wg)
	default:
		log.Fatalf("unknown -mode %q (want kqueue, kqueue-wbuf or net)", mode)
	}
	go server.WaitForSignal(&wg, signals)

	wg.Wait()
}
