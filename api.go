package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// apiClient registers this box-agent instance with the management API, so
// it can track which LLM model is currently being served. The instance
// (and its provider) is identified by the bearer token alone — the API
// looks it up server-side, so the request body carries only the model.
type apiClient struct {
	baseURL string
	token   string
	client  *http.Client
}

type registerModelRequest struct {
	Model string `json:"model"`
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

// registerModel tells the API backend which model this box-agent instance
// is currently serving.
func (a *apiClient) registerModel(model string) error {
	payload, err := json.Marshal(registerModelRequest{Model: model})
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
