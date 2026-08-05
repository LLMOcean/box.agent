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
	apiURL := flag.String("api-url", "http://localhost:8000", "base URL of the management API used to register this box-agent and sync its running model")
	apiToken := flag.String("api-token", os.Getenv("API_TOKEN"), "bearer token for the management API (or set API_TOKEN)")
	contextLength := flag.Int("context-length", 0, "context window size in tokens this backend supports (0 = unreported)")
	maxOutputLength := flag.Int("max-output-length", 0, "max output tokens this backend supports (0 = unreported)")
	quantization := flag.String("quantization", "", "quantization of the served model, e.g. \"fp8\", \"int4\" (empty = unreported)")
	inputModalities := flag.String("input-modalities", "text", "comma-separated input modalities this model accepts")
	outputModalities := flag.String("output-modalities", "text", "comma-separated output modalities this model produces")
	supportedFeatures := flag.String("supported-features", "", "comma-separated capability tags this backend supports, e.g. \"tools,json_mode\" (empty = none declared)")
	flag.Parse()

	if *provider == "" || *token == "" {
		log.Fatal("both -provider and -token (or AGENT_TOKEN env var) are required")
	}
	if *apiToken == "" {
		log.Fatal("-api-token (or API_TOKEN env var) is required")
	}

	connectURL := fmt.Sprintf("%s/v1/agents/connect?provider=%s", *routerURL, url.QueryEscape(*provider))
	backend := &llmBackend{baseURL: strings.TrimSuffix(*llmURL, "/"), apiKey: *llmAPIKey, modelOverride: *backendModel, client: &http.Client{}}

	model, err := backend.runningModel()
	if err != nil {
		log.Fatalf("determine running model: %v", err)
	}

	api := &apiClient{baseURL: strings.TrimSuffix(*apiURL, "/"), token: *apiToken, client: &http.Client{}}

	identity, err := api.authenticate()
	if err != nil {
		log.Fatalf("provider does not exist for this token: %v", err)
	}
	log.Printf("provider found: instance_name=%s", identity.InstanceName)

	if err := api.registerModel(model); err != nil {
		log.Fatalf("register with API: %v", err)
	}
	log.Printf("registered with API: model=%s", model)

	caps := &Capabilities{
		ContextLength:     *contextLength,
		MaxOutputLength:   *maxOutputLength,
		Quantization:      *quantization,
		InputModalities:   splitCSV(*inputModalities),
		OutputModalities:  splitCSV(*outputModalities),
		SupportedFeatures: splitCSV(*supportedFeatures),
	}

	for {
		if err := connectAndServe(connectURL, *token, backend, caps); err != nil {
			log.Printf("connection error: %v — reconnecting in 5s", err)
		}
		time.Sleep(5 * time.Second)
	}
}

// splitCSV splits a comma-separated flag value into a trimmed slice, or nil
// for an empty string (so it's omitted from the "hello" frame entirely
// rather than sent as an empty-but-present list).
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
