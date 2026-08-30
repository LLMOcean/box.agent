package main

import (
	"bytes"
	"fmt"
	json "github.com/goccy/go-json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// apiClient registers this box-agent instance with the management API, so
// it can track which LLM model is currently being served. The instance is
// identified by the bearer token alone — the API looks it up server-side.
// The provider is normally fixed on the instance already, but registerModel
// can also report it explicitly to move the instance to a different (or
// not-yet-existing) provider.
type apiClient struct {
	baseURL string
	token   string
	client  *http.Client
}

type registerModelRequest struct {
	Model            string  `json:"model"`
	Provider         string  `json:"provider,omitempty"`
	IsPublic         bool    `json:"is_public"`
	InputPerMillion  float64 `json:"input_per_million,omitempty"`
	OutputPerMillion float64 `json:"output_per_million,omitempty"`
	ContextLength    int     `json:"context_length,omitempty"`
	Quantization     string  `json:"quantization,omitempty"`
	HuggingFaceID    string  `json:"hugging_face_id,omitempty"`
}

// AgentIdentity is the orchestrator's per-token identity for this box-agent
// instance, as returned by POST /llmocean/agent-instances/authenticate.
// Mirrors router.agent's own AgentAuthResult — same endpoint, same shape.
type AgentIdentity struct {
	AgentInstanceID string `json:"id"`
	ProviderID      string `json:"llmocean_provider_id"`
	InstanceName    string `json:"instance_name"`
	IsActive        bool   `json:"is_active"`
	Status          string `json:"status"`
}

type agentIdentityEnvelope struct {
	Data AgentIdentity `json:"data"`
}

// authenticate resolves this instance's token to its provider identity via
// the same anonymous, token-only endpoint router.agent itself calls at
// WS-connect time. Used as a pre-flight check before registerModel, so a
// token with no provider behind it fails with a clear message instead of
// whatever register-model's side effects happen to produce for it.
func (a *apiClient) authenticate() (*AgentIdentity, error) {
	req, err := http.NewRequest(http.MethodPost, a.baseURL+"/llmocean/agent-instances/authenticate", nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api backend error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var envelope agentIdentityEnvelope
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if envelope.Data.ProviderID == "" || !envelope.Data.IsActive {
		return nil, fmt.Errorf("token has no active provider (instance_name=%q, is_active=%v)", envelope.Data.InstanceName, envelope.Data.IsActive)
	}
	return &envelope.Data, nil
}

type createAgentInstanceRequest struct {
	ProviderName string `json:"provider_name"`
	ModelName    string `json:"model_name,omitempty"`
}

type createAgentInstanceEnvelope struct {
	Data struct {
		Token        string `json:"token"`
		InstanceName string `json:"instance_name"`
	} `json:"data"`
}

// createAgentInstance provisions a brand-new agent instance via the
// management API and returns its one-time plaintext token. Unlike
// authenticate/registerModel (anonymous, per-instance-token routes), this
// endpoint requires a full IAM/account-level bearer token - a.token here
// must be that IAM token, not a per-instance one.
func (a *apiClient) createAgentInstance(providerName, modelName string) (string, error) {
	payload, err := json.Marshal(createAgentInstanceRequest{ProviderName: providerName, ModelName: modelName})
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, a.baseURL+"/llmocean/agent-instances", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)

	resp, err := a.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("api backend error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var envelope createAgentInstanceEnvelope
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}
	if envelope.Data.Token == "" {
		return "", fmt.Errorf("create response had no token")
	}
	return envelope.Data.Token, nil
}

// ensureAgentToken returns a per-instance agent token to register/connect
// with, reusing one cached at cachePath if it's still valid, or
// auto-provisioning a fresh one via provisioner (an apiClient authenticated
// with an IAM/account-level token) otherwise. Caching avoids spawning a new
// orphaned agent instance server-side on every restart (crash, systemctl
// restart, reboot, ...) - without it, every run with no explicit -token
// would provision a brand-new instance.
func ensureAgentToken(provisioner *apiClient, cachePath, providerName, modelName string) (string, error) {
	if cachePath != "" {
		if data, err := os.ReadFile(cachePath); err == nil {
			if cached := strings.TrimSpace(string(data)); cached != "" {
				probe := &apiClient{baseURL: provisioner.baseURL, token: cached, client: provisioner.client}
				if _, err := probe.authenticate(); err == nil {
					log.Printf("reusing cached agent token from %s", cachePath)
					return cached, nil
				} else {
					log.Printf("cached agent token at %s is no longer valid, provisioning a new one: %v", cachePath, err)
				}
			}
		}
	}

	log.Printf("provisioning a new agent instance for provider=%q model=%q", providerName, modelName)
	token, err := provisioner.createAgentInstance(providerName, modelName)
	if err != nil {
		return "", fmt.Errorf("create agent instance: %w", err)
	}

	if cachePath != "" {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0700); err != nil {
			log.Printf("warning: could not create token cache dir %s: %v", filepath.Dir(cachePath), err)
		} else if err := os.WriteFile(cachePath, []byte(token), 0600); err != nil {
			log.Printf("warning: could not write token cache %s: %v", cachePath, err)
		} else {
			log.Printf("cached new agent token at %s", cachePath)
		}
	}

	return token, nil
}

// registerModel tells the API backend which model (and, optionally, which
// provider) this box-agent instance is currently serving. Passing a
// non-empty provider lets the API move this instance to that provider,
// creating it server-side if it doesn't exist yet - see
// AgentInstancesService::registerModel() in the management API.
//
// isPublic, inputPerMillion, outputPerMillion, contextLength, quantization,
// and huggingFaceID are sent alongside the request and persisted server-side
// onto the llmocean_provider_models row (max_context_tokens/quantization/
// hugging_face_id columns) - only whichever of these a given call actually
// sets is written, so a partial report never clobbers previously-known
// metadata with zero/empty. contextLength/quantization are usually
// auto-detected (see ollamaModelInfo/llmBackend.modelInfo) rather than
// operator-supplied; huggingFaceID is derived from the model name itself
// (see huggingFaceRepoID) and is "" when it can't be determined - a zero/""
// value here is simply omitted from the request (see registerModelRequest's
// omitempty tags) rather than sent and overwriting a previously-known value.
func (a *apiClient) registerModel(model, provider string, isPublic bool, inputPerMillion, outputPerMillion float64, contextLength int, quantization, huggingFaceID string) error {
	payload, err := json.Marshal(registerModelRequest{
		Model:            model,
		Provider:         provider,
		IsPublic:         isPublic,
		InputPerMillion:  inputPerMillion,
		OutputPerMillion: outputPerMillion,
		ContextLength:    contextLength,
		Quantization:     quantization,
		HuggingFaceID:    huggingFaceID,
	})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, a.baseURL+"/llmocean/agent-instances/register-model", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api backend error (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// reportStatusRequest is a merge-patch snapshot of this instance's current
// state - unlike reportMetricsRequest below, a zero/unknown field here must
// never overwrite previously-known server-side state, same reasoning
// registerModelRequest's own omitempty fields already use. Sent both
// immediately on every BackendState transition (so "downloading the model"/
// "booting" is visible close to live, not just on the next periodic tick)
// and periodically as a steady-state heartbeat - see reporter.go's
// reportStatusLoop, the only caller.
type reportStatusRequest struct {
	State                       BackendState       `json:"state"`
	AgentVersion                string             `json:"agent_version,omitempty"`
	AgentUptimeSeconds          float64            `json:"agent_uptime_seconds,omitempty"`
	Host                        HostStatus         `json:"host,omitempty"`
	Backend                     BackendStatus      `json:"backend,omitempty"`
	VLLMProcess                 *VLLMProcessStatus `json:"vllm_process,omitempty"`
	RouterConnections           int                `json:"router_connections"` // no omitempty - 0 live is the alarm signal
	RouterConnectionsConfigured int                `json:"router_connections_configured,omitempty"`
	ReportedAt                  time.Time          `json:"reported_at"`
}

// reportStatus tells the API backend this instance's current lifecycle
// state and a resource/backend snapshot - see reportStatusRequest's doc
// comment for the merge-patch semantics this relies on.
func (a *apiClient) reportStatus(reqBody reportStatusRequest) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, a.baseURL+"/llmocean/agent-instances/report-status", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api backend error (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// reportMetricsRequest is one independent time-series event describing
// "what happened in this window" - the opposite semantics from
// reportStatusRequest above: RequestCount of 0 is real, meaningful data (an
// idle box) and must never be omitted or merged away, so this endpoint is
// sent unconditionally every window regardless of traffic. See
// reporter.go's reportMetricsLoop, the only caller, and requestWindow.
// snapshot (metrics.go), which builds this struct.
type reportMetricsRequest struct {
	WindowStart   time.Time `json:"window_start"`
	WindowSeconds float64   `json:"window_seconds"`

	RequestCount int64 `json:"request_count"` // never omitempty - 0 is meaningful
	ErrorCount   int64 `json:"error_count"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`

	AvgInputTokensPerRequest  float64 `json:"avg_input_tokens_per_request,omitempty"`
	AvgOutputTokensPerRequest float64 `json:"avg_output_tokens_per_request,omitempty"`
	// TokensPerSecond is omitted (not sent as zero) when RequestCount==0 -
	// undefined for an idle window, not "0 tok/s".
	TokensPerSecond float64 `json:"tokens_per_second,omitempty"`

	NonStreamCount        int64   `json:"non_streaming_count"`
	NonStreamLatencyAvgMs float64 `json:"non_streaming_latency_avg_ms,omitempty"`
	// NonStreamLatencyP95Ms is a bucket-resolution approximation, not an
	// exact percentile - see p95FromBuckets (metrics.go).
	NonStreamLatencyP95Ms float64 `json:"non_streaming_latency_p95_ms,omitempty"`

	StreamCount     int64   `json:"streaming_count"`
	StreamTTFTAvgMs float64 `json:"streaming_ttft_avg_ms,omitempty"`
	StreamTTFTP95Ms float64 `json:"streaming_ttft_p95_ms,omitempty"` // also bucket-resolution, see above
}

// reportMetrics flushes one reporting window's aggregated request
// performance to the API - see reportMetricsRequest's doc comment for why
// this is sent unconditionally, including all-zero windows.
func (a *apiClient) reportMetrics(reqBody reportMetricsRequest) error {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, a.baseURL+"/llmocean/agent-instances/report-metrics", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api backend error (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

type reportHealthRequest struct {
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

// reportHealth tells the API backend whether this instance's local LLM
// backend is currently reachable - see monitorBackendHealth in backend.go,
// the only caller, which reports a transition (not every probe) so a
// healthy steady state never spams this endpoint. detail is the probe's raw
// error text on an unhealthy report, empty on a healthy one.
func (a *apiClient) reportHealth(healthy bool, detail string) error {
	payload, err := json.Marshal(reportHealthRequest{Healthy: healthy, Detail: detail})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, a.baseURL+"/llmocean/agent-instances/report-health", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+a.token)

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api backend error (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}
