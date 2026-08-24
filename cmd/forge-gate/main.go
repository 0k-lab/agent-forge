package main

import (
	"flag"
	"log"
	"net/http"

	"agent-forge/internal/gate"
	"agent-forge/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	db := flag.String("db", "forge.db", "SQLite path")
	workerID := flag.String("worker-id", "worker-1", "authorized worker ID")
	token := flag.String("worker-token", "dev-token", "worker bearer token")
	flag.Parse()
	s, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	log.Printf("forge-gate listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, gate.NewHandler(s, map[string]string{*token: *workerID})))
}
