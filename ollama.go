package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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
// at llmURL via OLLAMA_HOST when that's not just ollama's own default.
func ollamaEnv(llmURL string) []string {
	host := strings.TrimPrefix(strings.TrimPrefix(llmURL, "http://"), "https://")
	if host == "" || host == "localhost:11434" {
		return os.Environ()
	}
	return append(os.Environ(), "OLLAMA_HOST="+host)
}

// pullModel installs model by running `ollama pull`, which resolves the
// name against Ollama's own library (or Hugging Face directly, for an
// "hf.co/<repo>[:<quant>]" name) and renders its own progress bar --
// streamed straight through to our stdout/stderr rather than reimplemented.
func pullModel(llmURL, model string) error {
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
	cmd := exec.Command("ollama", "rm", model)
	cmd.Env = ollamaEnv(llmURL)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ollama rm %s: %w", model, err)
	}
	return nil
}
