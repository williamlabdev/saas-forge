package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/williamlabdev/saas-forge/internal/pkg/mcp/mock"
)

func main() {
	addr := getenv("MCP_MOCK_ADDR", ":8081")
	srv := mock.NewServer()
	log.Printf("mcp-mock listening on %s (PUT /v1/users/state)", addr)
	server := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
