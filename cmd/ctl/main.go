package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
)

func main() {
	server := flag.String("server", "localhost:7101", "proxxxy-server stats address")
	flag.Parse()

	conn, err := net.Dial("tcp", *server)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	var stats map[string]interface{}
	if err := json.NewDecoder(conn).Decode(&stats); err != nil {
		log.Fatal(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(stats); err != nil {
		log.Fatal(err)
	}
	fmt.Println()
}
