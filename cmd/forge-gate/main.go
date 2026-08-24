package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"agent-forge/internal/gate"
	"agent-forge/internal/store"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	db := flag.String("db", "forge.db", "SQLite path")
	workerID := flag.String("worker-id", "worker-1", "authorized worker ID")
	workerToken := flag.String("worker-token", os.Getenv("FORGE_WORKER_TOKEN"), "worker bearer token (default FORGE_WORKER_TOKEN)")
	ownerToken := flag.String("owner-token", os.Getenv("FORGE_OWNER_TOKEN"), "owner bearer token (default FORGE_OWNER_TOKEN)")
	flag.Parse()
	if *workerToken == "" || *ownerToken == "" || *workerToken == *ownerToken {
		log.Fatal("distinct non-empty worker and owner tokens are required")
	}
	s, err := store.Open(*db)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	log.Printf("forge-gate listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, gate.NewHandler(s, map[string]string{*workerToken: *workerID}, *ownerToken)))
}
