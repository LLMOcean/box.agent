package main

import (
	"bytes"
	"fmt"
	json "github.com/goccy/go-json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ensureOllamaInstalled makes sure the ollama CLI/daemon is present on this
// box, installing it via Ollama's official install script if not. The
// script installs to /usr/local/bin and registers a systemd service, so it
// needs root -- box.agent's own provisioning already assumes root access,
// so this runs the script directly rather than trying to detect/escalate
// privileges itself. If it's not run as root, the resulting permission
// error from the script is self-explanatory.
func ensureOllamaInstalled() error {
	if _, err := exec.LookPath("ollama"); err == nil {
		return nil
	}

	fmt.Println("ollama not found -- installing via https://ollama.com/install.sh ...")
	cmd := exec.Command("sh", "-c", "curl -fsSL https://ollama.com/install.sh | sh")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run ollama install script: %w", err)
	}

	if _, err := exec.LookPath("ollama"); err != nil {
		return fmt.Errorf("ollama still not found on PATH after running install script")
	}
	return nil
}

// ollamaEnv returns the environment for an `ollama` subprocess, pointing it
// at llmURL via OLLAMA_HOST - always set (rather than left to ollama's own
// built-in default of localhost:11434), so wherever `ollama serve` ends up
// listening always matches -llm-url, whatever that's set to.
func ollamaEnv(llmURL string) []string {
	host := strings.TrimPrefix(strings.TrimPrefix(llmURL, "http://"), "https://")
	if host == "" {
		return os.Environ()
	}
	return append(os.Environ(), "OLLAMA_HOST="+host)
}

// ollamaBaseURL derives the HTTP base URL ollama's server should be
// reachable at from llmURL - same host/port convention as ollamaEnv's
// OLLAMA_HOST, just formatted as a URL for a plain readiness check.
func ollamaBaseURL(llmURL string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(llmURL, "http://"), "https://")
	if host == "" {
		host = "localhost:8000"
	}
	return "http://" + host
}

// ollamaServerUp reports whether an ollama server is already answering at
// llmURL.
func ollamaServerUp(llmURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(ollamaBaseURL(llmURL))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ollamaShowResponse is the subset of Ollama's POST /api/show response
// (https://github.com/ollama/ollama/blob/main/docs/api.md#show-model-information)
// this agent reads to auto-detect capability info it would otherwise need
// an operator to pass by hand via -context-length/-quantization.
// ModelInfo's keys are architecture-prefixed (e.g. "llama.context_length",
// "qwen2.context_length") rather than one fixed name, so it's decoded as a
// raw map and scanned for the matching suffix instead of a fixed field.
type ollamaShowResponse struct {
	Details struct {
		QuantizationLevel string `json:"quantization_level"`
	} `json:"details"`
	ModelInfo map[string]json.RawMessage `json:"model_info"`
}

// ollamaModelInfo asks a running Ollama server what it knows about model's
// context length and quantization, so callers can self-report accurate
// capabilities without an operator having to pass -context-length/
// -quantization by hand. Returns zero values (never an error) if the server
// isn't reachable, doesn't recognize model, or isn't Ollama at all (e.g. a
// bare vLLM backend, which has no /api/show) - callers should treat that as
// "unknown" and fall back to whatever flags were given, exactly as if
// auto-detection didn't exist.
func ollamaModelInfo(llmURL, model string) (contextLength int, quantization string) {
	client := &http.Client{Timeout: 5 * time.Second}
	payload, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return 0, ""
	}
	resp, err := client.Post(ollamaBaseURL(llmURL)+"/api/show", "application/json", bytes.NewReader(payload))
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, ""
	}

	var show ollamaShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&show); err != nil {
		return 0, ""
	}

	quantization = show.Details.QuantizationLevel
	for key, raw := range show.ModelInfo {
		if !strings.HasSuffix(key, ".context_length") {
			continue
		}
		var n int
		if err := json.Unmarshal(raw, &n); err == nil {
			contextLength = n
		}
		break
	}
	return contextLength, quantization
}

// huggingFaceRepoID extracts a Hugging Face repo id from model, when the
// model name itself carries one - the only source this agent has for it,
// since neither Ollama's /api/show nor vLLM's API reports it separately.
// Two conventions in play, both already documented elsewhere in this repo:
// Ollama's "hf.co/<repo>[:<quant tag>]" direct-pull form (see pullModel
// above), and vLLM's -backend-model convention of the bare repo id itself,
// e.g. "tcclaviger/Qwen3.6-40B-..." (see the -backend-model flag docs).
// Returns "" for a plain Ollama library name (e.g. "llama3", "llama3:8b"),
// which isn't a Hugging Face repo at all.
func huggingFaceRepoID(model string) string {
	repo := model
	if strings.HasPrefix(strings.ToLower(repo), "hf.co/") {
		repo = repo[len("hf.co/"):]
	} else if !strings.Contains(repo, "/") {
		return ""
	}
	if idx := strings.LastIndex(repo, ":"); idx != -1 {
		repo = repo[:idx]
	}
	return repo
}

// ensureOllamaRunning starts `ollama serve` in the background if no server
// is already answering at llmURL. Ollama's install script registers a
// systemd service for this, but that's a no-op in environments with no
// systemd running - containers and minimal cloud VPS images, exactly where
// box-agent commonly gets deployed - leaving `ollama pull`/`ollama rm` with
// nothing to connect to ("could not connect to ollama server").
func ensureOllamaRunning(llmURL string) error {
	if ollamaServerUp(llmURL) {
		return nil
	}

	fmt.Println("ollama server not reachable -- starting \"ollama serve\" in the background...")
	logFile, err := os.OpenFile("/tmp/ollama-serve.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open ollama serve log: %w", err)
	}

	cmd := exec.Command("ollama", "serve")
	cmd.Env = ollamaEnv(llmURL)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ollama serve: %w", err)
	}
	// Deliberately not Wait()'d - ollama serve is meant to keep running in
	// the background for install-model/pullModel/deleteModel (and, in the
	// systemd deployment, would normally be supervised by its own unit
	// instead of us at all).

	for i := 0; i < 30; i++ {
		if ollamaServerUp(llmURL) {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("ollama server did not become ready within 15s of starting (see /tmp/ollama-serve.log)")
}

// pullModel installs model by running `ollama pull`, which resolves the
// name against Ollama's own library (or Hugging Face directly, for an
// "hf.co/<repo>[:<quant>]" name) and renders its own progress bar --
// streamed straight through to our stdout/stderr rather than reimplemented.
func pullModel(llmURL, model string) error {
	if err := ensureOllamaRunning(llmURL); err != nil {
		return err
	}

	cmd := exec.Command("ollama", "pull", model)
	cmd.Env = ollamaEnv(llmURL)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ollama pull %s: %w", model, err)
	}
	return nil
}

// deleteModel removes an installed model by running `ollama rm`.
func deleteModel(llmURL, model string) error {
	if err := ensureOllamaRunning(llmURL); err != nil {
		return err
	}

	cmd := exec.Command("ollama", "rm", model)
	cmd.Env = ollamaEnv(llmURL)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ollama rm %s: %w", model, err)
	}
	return nil
}
