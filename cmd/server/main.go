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
	display := flag.Int("display", 2, "X display number to present (e.g. 2 → DISPLAY=:2)")
	port := flag.Int("port", 7100, "TCP port for proxxxy-client")
	flag.Parse()

	s := server.New(*display, *port)
	if err := s.Start(); err != nil {
		log.Fatal(err)
	}
	log.Printf("DISPLAY=:%d  client port=%d", *display, *port)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	s.Stop()
}
