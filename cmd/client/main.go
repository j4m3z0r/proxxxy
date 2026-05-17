package main

import (
	"flag"
	"log"

	"james.id.au/proxxxy/internal/client"
)

func main() {
	server := flag.String("server", "localhost:7100", "proxxxy-server address")
	flag.Parse()

	c := client.New(*server)
	if err := c.Run(); err != nil {
		log.Fatal(err)
	}
}
