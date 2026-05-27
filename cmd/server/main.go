package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"james.id.au/proxxxy/internal/server"
)

func main() {
	display    := flag.Int("display", 2, "X display number to present (e.g. 2 → DISPLAY=:2)")
	port       := flag.Int("port", 7100, "TCP port for proxxxy-client")
	statsPort  := flag.Int("stats-port", 0, "HTTP stats port (0 = port+1)")
	listenAddr := flag.String("listen", "127.0.0.1", "address to bind the TCP listener (e.g. 0.0.0.0 for all interfaces)")
	flag.Parse()

	sp := *statsPort
	if sp == 0 {
		sp = *port + 1
	}

	s := server.New(*display, *port, sp, *listenAddr)
	if err := s.Start(); err != nil {
		log.Fatal(err)
	}
	log.Printf("DISPLAY=:%d  client-port=%d  stats=http://localhost:%d/stats",
		*display, *port, sp)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	s.Stop()
}
