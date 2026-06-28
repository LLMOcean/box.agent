// Command box-agent is a remote WebSocket agent for router.agent. It dials
// out to a router's /v1/agents/connect endpoint, authenticates with a
// static bearer token, and forwards every chat request it receives to a
// local OpenAI-compatible LLM server (Ollama, vLLM, llama.cpp-server, ...),
// translating the HTTP response back into the router's wire frames.
//
// See router.agent's docs/AGENT_PROTOCOL.md and docs/AGENT_BUILD_SPEC.md
// for the protocol and design this implements.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	routerURL := flag.String("router", "ws://localhost:8080", "router base URL (ws:// or wss://)")
	provider := flag.String("provider", "", "agent provider name (must match an agents: key in the router's config.yaml)")
	token := flag.String("token", os.Getenv("AGENT_TOKEN"), "shared token for this provider (or set AGENT_TOKEN)")
	llmURL := flag.String("llm-url", "http://localhost:11434", "base URL of the local OpenAI-compatible LLM server (Ollama/vLLM)")
	llmAPIKey := flag.String("llm-api-key", os.Getenv("LLM_API_KEY"), "bearer token for the local LLM server, if it requires one")
	backendModel := flag.String("backend-model", "", "override the model name sent to the local LLM backend (use when the backend's own model id differs from the router's registered model name, e.g. vLLM expects the full repo id it was started with)")
	flag.Parse()

	if *provider == "" || *token == "" {
		log.Fatal("both -provider and -token (or AGENT_TOKEN env var) are required")
	}

	connectURL := fmt.Sprintf("%s/v1/agents/connect?provider=%s", *routerURL, url.QueryEscape(*provider))
	backend := &llmBackend{baseURL: strings.TrimSuffix(*llmURL, "/"), apiKey: *llmAPIKey, modelOverride: *backendModel, client: &http.Client{}}

	for {
		if err := connectAndServe(connectURL, *token, backend); err != nil {
			log.Printf("connection error: %v — reconnecting in 5s", err)
		}
		time.Sleep(5 * time.Second)
	}
}
