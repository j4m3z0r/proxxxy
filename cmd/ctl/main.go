package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
)

func main() {
	server    := flag.String("server",    "http://localhost:7101", "proxxxy stats server base URL")
	aggregate := flag.Bool("aggregate",   false,                   "show aggregate stats only")
	flag.Parse()

	url := *server + "/stats"
	if *aggregate {
		url += "?aggregate=1"
	}

	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}

	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		log.Fatal(err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}
