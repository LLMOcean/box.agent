// Command box-agent is a remote WebSocket agent for router.agent. It dials
// out to a router's /v1/agents/connect endpoint, authenticates with a
// static bearer token, and forwards every chat request it receives to a
// local OpenAI-compatible LLM server (Ollama, vLLM, llama.cpp-server, ...),
// translating the HTTP response back into the router's wire frames.
//
// See router.agent's docs/AGENT_PROTOCOL.md and docs/AGENT_BUILD_SPEC.md
// for the protocol and design this implements.
//
// Two one-shot subcommands manage models on a local Ollama server directly
// (no router involvement -- run these on the box itself, as root: installing
// ollama itself, if it isn't already, needs it):
//
//	box-agent install-model [-llm-url URL] <model>
//	box-agent delete-model  [-llm-url URL] <model>
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

	"github.com/LLMOcean/box.agent/version"
)

func main() {
	// install-model/delete-model are recognized only as the literal first
	// argument, so any daemon invocation starting with a "-flag" (the only
	// form used before these subcommands existed) still falls through to
	// runAgent unchanged.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install-model":
			runInstallModel(os.Args[2:])
			return
		case "delete-model":
			runDeleteModel(os.Args[2:])
			return
		}
	}
	runAgent()
}

func runAgent() {
	routerURL := flag.String("router", "wss://llm.greenference.com", "router base URL (ws:// or wss://)")
	provider := flag.String("provider", "", "provider name to register with the router, e.g. \"plusclouds\" (the running backend model is appended automatically as \"provider/model\" unless this already contains a \"/\")")
	token := flag.String("token", os.Getenv("AGENT_TOKEN"), "an existing per-instance agent token (or set AGENT_TOKEN) - also used as -api-token unless that's set separately. If unset, -api-token must be an IAM/account-level token instead, which box-agent uses to auto-provision a new agent instance (and its token) on first run")
	llmURL := flag.String("llm-url", "http://localhost:8000", "base URL of the local OpenAI-compatible LLM server (Ollama/vLLM)")
	llmAPIKey := flag.String("llm-api-key", os.Getenv("LLM_API_KEY"), "bearer token for the local LLM server, if it requires one")
	backendModel := flag.String("backend-model", "", "model name to register and to send to the local LLM backend (required) - e.g. vLLM expects the full repo id it was started with, like \"tcclaviger/Qwen3.6-40B-...\"")
	installModel := flag.Bool("install-model", false, "pull -backend-model via Ollama (using -llm-url) before starting, if it isn't already installed - equivalent to running \"box-agent install-model\" first, but as one step")
	apiURL := flag.String("api-url", "https://api.greenference.com", "base URL of the management API used to register this box-agent and sync its running model")
	apiToken := flag.String("api-token", os.Getenv("API_TOKEN"), "bearer token for the management API (or set API_TOKEN); defaults to -token if not set. When -token itself is unset, this must be an IAM/account-level token used to auto-provision an agent instance instead")
	tokenCache := flag.String("token-cache", "/var/lib/box-agent/token", "local file to cache an auto-provisioned agent token across restarts, so box-agent doesn't provision a new agent instance every run - only used when -token isn't set. Set to empty to disable caching")
	isPublic := flag.Bool("is-public", true, "whether this model/provider should be publicly listed (sent as is_public on registration)")
	inputPerMillion := flag.Float64("input-per-million", 0, "input price per million tokens to report at registration (0 = unreported)")
	outputPerMillion := flag.Float64("output-per-million", 0, "output price per million tokens to report at registration (0 = unreported)")
	contextLength := flag.Int("context-length", 0, "context window size in tokens this backend supports (0 = unreported)")
	maxOutputLength := flag.Int("max-output-length", 0, "max output tokens this backend supports (0 = unreported)")
	quantization := flag.String("quantization", "", "quantization of the served model, e.g. \"fp8\", \"int4\" (empty = unreported)")
	inputModalities := flag.String("input-modalities", "text", "comma-separated input modalities this model accepts")
	outputModalities := flag.String("output-modalities", "text", "comma-separated output modalities this model produces")
	supportedFeatures := flag.String("supported-features", "", "comma-separated capability tags this backend supports, e.g. \"tools,json_mode\" (empty = none declared)")
	connections := flag.Int("connections", 1, "number of parallel WebSocket connections to open to the router, all under the same -provider/-token, all proxying to the same -llm-url - an experiment for isolating whether one connection's write-serialization (see router.agent's Conn.writeMu) is ever a real bottleneck versus the backend's own concurrency ceiling; most deployments should leave this at 1 and instead run separate box-agent processes per independent backend (see README)")
	showVersion := flag.Bool("version", false, "print the build version and exit - compares a running/downloaded binary against what's actually published (git tags and GitHub Releases are different things; a stale binary silently missing newly-added flags, like this one used to, is usually a release-vs-tag mismatch)")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	log.Printf("box-agent version %s", version.Version)

	if *connections < 1 {
		log.Fatal("-connections must be >= 1")
	}

	if *provider == "" || *backendModel == "" {
		log.Fatal("-provider and -backend-model are required - box-agent does not guess these")
	}
	if *token == "" && *apiToken == "" {
		log.Fatal("either -token (an existing agent token) or -api-token (an IAM/account token to auto-provision one) is required")
	}

	if *installModel {
		log.Printf("installing model %q via Ollama before starting", *backendModel)
		if err := ensureOllamaInstalled(); err != nil {
			log.Fatalf("install model: %v", err)
		}
		if err := pullModel(*llmURL, *backendModel); err != nil {
			log.Fatalf("install model: %v", err)
		}
		log.Printf("installed %q", *backendModel)
	}

	// -token and -api-token are the same value in practice (one
	// registration token authenticates both the router connection and the
	// management API) - fall back to -token so operators only pass one,
	// while still letting -api-token/API_TOKEN override it if they ever
	// diverge. Only applies when -token was actually given directly; when
	// it wasn't, -api-token is an IAM token for provisioning instead, not a
	// per-instance one, so it must never be used as this fallback.
	if *apiToken == "" && *token != "" {
		apiToken = token
	}

	// backendTransport raises MaxIdleConnsPerHost well past Go's default of
	// 2 - every concurrent chat/stream request this agent handles (see
	// connection.go's per-request goroutine) opens its own connection to
	// -llm-url, all to the same host. At the default, anything past the
	// first 2 concurrent requests gets its connection torn down after use
	// instead of pooled, paying a fresh TCP handshake next time instead of
	// reusing one - cheap on localhost, but a real, avoidable cost under
	// sustained concurrency, and non-trivial if -llm-url is a different host
	// on the LAN rather than localhost.
	backendTransport := http.DefaultTransport.(*http.Transport).Clone()
	backendTransport.MaxIdleConnsPerHost = 128
	backend := &llmBackend{baseURL: strings.TrimSuffix(*llmURL, "/"), apiKey: *llmAPIKey, modelOverride: *backendModel, client: &http.Client{Transport: backendTransport}}

	// -backend-model is the model, full stop - box-agent used to fall back
	// to GET {-llm-url}/v1/models and guess from whatever came back first
	// when this was unset, which silently registered garbage if -llm-url
	// pointed at anything other than a genuine single-model backend (e.g.
	// a multi-model catalog/router). Require it explicitly instead.
	model := *backendModel

	// The router's provider namespace is "provider/model" (see its
	// docs/AGENT_PROTOCOL.md and handlers.AgentWSHandler) - fold the
	// model into -provider automatically so a bare "-provider foo" doesn't
	// register under just "foo" with no model segment, forcing every
	// downstream lookup (router.agent's /v1/agents/status, GET /v1/models
	// readiness, /v1/chat routing) to mismatch against the orchestrator's
	// own "provider/model" catalog ids. Skip it if the operator already
	// passed the combined form themselves.
	providerName := *provider
	if !strings.Contains(providerName, "/") {
		providerName = providerName + "/" + model
	}
	connectURL := fmt.Sprintf("%s/v1/agents/connect?provider=%s", *routerURL, url.QueryEscape(providerName))

	// The management API's provider is a bare name (e.g. "plusclouds"), not
	// the router's "provider/model" namespace - strip off anything after the
	// first "/" in case the operator already passed the combined form to
	// -provider.
	apiProviderName := *provider
	if idx := strings.Index(apiProviderName, "/"); idx != -1 {
		apiProviderName = apiProviderName[:idx]
	}

	// agentToken is the per-instance token that authenticates the router WS
	// connect specifically. apiCallToken is what authenticates
	// authenticate()/registerModel() - normally the same value, except right
	// after auto-provisioning, where the operator's -api-token was an
	// IAM/account token with no standing on those anonymous,
	// per-instance-token routes and must be swapped for the new instance
	// token too.
	agentToken := *token
	apiCallToken := *apiToken
	if agentToken == "" {
		provisioner := &apiClient{baseURL: strings.TrimSuffix(*apiURL, "/"), token: *apiToken, client: &http.Client{}}
		newToken, err := ensureAgentToken(provisioner, *tokenCache, apiProviderName, model)
		if err != nil {
			log.Fatalf("provision agent token: %v", err)
		}
		agentToken = newToken
		apiCallToken = newToken
	}

	api := &apiClient{baseURL: strings.TrimSuffix(*apiURL, "/"), token: apiCallToken, client: &http.Client{}}

	identity, err := api.authenticate()
	if err != nil {
		log.Fatalf("provider does not exist for this token: %v", err)
	}
	log.Printf("provider found: instance_name=%s", identity.InstanceName)

	if err := api.registerModel(model, apiProviderName, *isPublic, *inputPerMillion, *outputPerMillion); err != nil {
		log.Fatalf("register with API: %v", err)
	}
	log.Printf("registered with API: model=%s provider=%s is_public=%v", model, apiProviderName, *isPublic)

	caps := &Capabilities{
		ContextLength:     *contextLength,
		MaxOutputLength:   *maxOutputLength,
		Quantization:      *quantization,
		InputModalities:   splitCSV(*inputModalities),
		OutputModalities:  splitCSV(*outputModalities),
		SupportedFeatures: splitCSV(*supportedFeatures),
	}

	runConnLoop := func(connIdx int) {
		for {
			if err := connectAndServe(connectURL, agentToken, backend, caps); err != nil {
				log.Printf("connection %d/%d error: %v — reconnecting in 5s", connIdx, *connections, err)
			}
			time.Sleep(5 * time.Second)
		}
	}

	// Connections 2..N run in their own goroutine; connection 1 runs in this
	// one, so a single-connection run (the default and common case) behaves
	// exactly as before - main() blocks here forever either way.
	for i := 2; i <= *connections; i++ {
		go runConnLoop(i)
	}
	runConnLoop(1)
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

// runInstallModel is the "box-agent install-model" subcommand: makes sure
// ollama itself is installed (running its official install script if not —
// requires root), then runs `ollama pull` for model.
func runInstallModel(args []string) {
	fs := flag.NewFlagSet("install-model", flag.ExitOnError)
	llmURL := fs.String("llm-url", "http://localhost:8000", "base URL of the local Ollama server")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: box-agent install-model [-llm-url URL] <model>")
		os.Exit(2)
	}
	model := fs.Arg(0)

	if err := ensureOllamaInstalled(); err != nil {
		log.Fatalf("install failed: %v", err)
	}
	if err := pullModel(*llmURL, model); err != nil {
		log.Fatalf("install failed: %v", err)
	}
	log.Printf("installed %q", model)
}

// runDeleteModel is the "box-agent delete-model" subcommand: runs
// `ollama rm` for model.
func runDeleteModel(args []string) {
	fs := flag.NewFlagSet("delete-model", flag.ExitOnError)
	llmURL := fs.String("llm-url", "http://localhost:8000", "base URL of the local Ollama server")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: box-agent delete-model [-llm-url URL] <model>")
		os.Exit(2)
	}
	model := fs.Arg(0)

	if err := deleteModel(*llmURL, model); err != nil {
		log.Fatalf("delete failed: %v", err)
	}
	log.Printf("deleted %q", model)
}
