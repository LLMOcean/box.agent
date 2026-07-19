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
