package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

func main() {
	port := flag.Int("port", 11435, "listen port (simulates Ollama)")
	upstream := flag.String("upstream", "http://localhost:8080", "upstream OpenAI-compatible API base URL")
	key := flag.String("api-key", "", "API key for upstream (optional)")
	debugFlag := flag.Bool("debug", false, "enable verbose debug logging")
	flag.Parse()

	upstreamURL = *upstream
	apiKey = *key
	debug = *debugFlag

	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", handleOllamaChat)
	mux.HandleFunc("/api/generate", handleOllamaGenerate)
	mux.HandleFunc("/api/embed", handleOllamaEmbed)
	mux.HandleFunc("/api/tags", handleOllamaTags)

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("ollama2oai listening on %s", addr)
	log.Printf("  Ollama routes → %s (OpenAI-compatible)", upstreamURL)
	log.Printf("  POST /api/chat     → POST /v1/chat/completions")
	log.Printf("  POST /api/generate → POST /v1/chat/completions (prompt wrapped as user message)")
	log.Printf("  POST /api/embed    → POST /v1/embeddings")
	log.Printf("  GET  /api/tags     → GET  /v1/models")

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
