package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// vllmConfig holds everything needed to launch, and re-launch, a single
// `vllm serve` child process for -backend-model. One box-agent process owns
// exactly one vLLM child, bound to -llm-url's own port - deploying more than
// one model on a box means running more than one box-agent process, each
// with its own -llm-url port, the same pattern deploy/install-and-run-multi.sh
// already uses to run several router registrations from one box.
type vllmConfig struct {
	Repo                 string // Hugging Face repo id - always -backend-model
	ServedModelName      string
	Host                 string
	Port                 string
	GPUMemUtil           float64
	MaxModelLen          int
	EnableSleepMode      bool
	EnableAutoToolChoice bool
	ToolCallParser       string
	ChatTemplate         string
	ExtraArgs            string
	HFToken              string
	HFHome               string
	LogPath              string
}

// vllmPortFromURL derives the port `vllm serve` should bind to straight from
// -llm-url, rather than taking a second, independent port flag that could
// drift out of sync with what box-agent itself then tries to talk to.
func vllmPortFromURL(llmURL string) (string, error) {
	u, err := url.Parse(llmURL)
	if err != nil {
		return "", fmt.Errorf("parse -llm-url: %w", err)
	}
	port := u.Port()
	if port == "" {
		return "", fmt.Errorf("-llm-url %q has no explicit port - -deploy-vllm needs one to bind vllm serve to (e.g. http://localhost:8000)", llmURL)
	}
	return port, nil
}

// ensureVLLMInstalled checks vllm is on PATH. Unlike ensureOllamaInstalled
// in ollama.go, this does not auto-install: vllm needs a CUDA toolkit/driver
// and a pinned Python/torch stack matched to the box, not a one-line curl
// script - silently pip-installing the wrong build would fail in more
// confusing ways than just erroring out here.
func ensureVLLMInstalled() error {
	if _, err := exec.LookPath("vllm"); err == nil {
		return nil
	}
	return fmt.Errorf("vllm not found on PATH - install it first (pip install vllm, matched to this box's CUDA/driver), then rerun with -deploy-vllm")
}

// downloadModel pulls repo via the `hf` CLI, falling back to the older
// `huggingface-cli` name - same fallback order mega-deploy.sh's dl_one uses,
// since either binary may be the one actually on PATH depending on the
// image. Streams progress straight to stdout/stderr rather than
// reimplementing it, the same choice ollama.go's pullModel already makes.
func downloadModel(repo, hfToken, hfHome string) error {
	env := os.Environ()
	if hfToken != "" {
		env = append(env, "HF_TOKEN="+hfToken, "HUGGING_FACE_HUB_TOKEN="+hfToken)
	}
	if hfHome != "" {
		env = append(env, "HF_HOME="+hfHome)
	}

	run := func(bin string, args ...string) error {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}

	log.Printf("downloading model %q via hf download ...", repo)
	if err := run("hf", "download", repo); err == nil {
		return nil
	}
	log.Printf("hf download unavailable or failed, falling back to huggingface-cli ...")
	if err := run("huggingface-cli", "download", repo); err != nil {
		return fmt.Errorf("download %s: %w", repo, err)
	}
	return nil
}

// args builds the vllm serve argument list. Booleans/parsers/templates are
// only appended when set, so an unconfigured field is simply absent rather
// than sent with a zero value vllm would reject or misinterpret.
func (c vllmConfig) args() []string {
	args := []string{
		"serve", c.Repo,
		"--served-model-name", c.ServedModelName,
		"--port", c.Port,
		"--host", c.Host,
	}
	if c.GPUMemUtil > 0 {
		args = append(args, "--gpu-memory-utilization", strconv.FormatFloat(c.GPUMemUtil, 'f', -1, 64))
	}
	if c.MaxModelLen > 0 {
		args = append(args, "--max-model-len", strconv.Itoa(c.MaxModelLen))
	}
	if c.EnableSleepMode {
		args = append(args, "--enable-sleep-mode")
	}
	if c.EnableAutoToolChoice {
		args = append(args, "--enable-auto-tool-choice")
	}
	if c.ToolCallParser != "" {
		args = append(args, "--tool-call-parser", c.ToolCallParser)
	}
	if c.ChatTemplate != "" {
		args = append(args, "--chat-template", c.ChatTemplate)
	}
	if c.ExtraArgs != "" {
		args = append(args, strings.Fields(c.ExtraArgs)...)
	}
	return args
}

// vllmSupervisor runs `vllm serve` in a restart-on-exit loop, mirroring the
// bash "while true; do vllm serve ...; sleep 10; done" wrapper mega-deploy.sh
// generates per model (see gen_serve there) - same restart-on-crash
// contract, just owned by box-agent itself instead of a generated shell
// script. kill forces the current child to exit early so the loop relaunches
// it immediately - how monitorBackendHealth (backend.go) recovers a
// hung-but-still-connected backend that a plain crash-restart would never
// catch.
type vllmSupervisor struct {
	cfg vllmConfig

	mu  sync.Mutex
	cmd *exec.Cmd
}

func newVLLMSupervisor(cfg vllmConfig) *vllmSupervisor {
	return &vllmSupervisor{cfg: cfg}
}

// run starts the restart-on-exit loop and blocks forever - call it in its
// own goroutine.
func (s *vllmSupervisor) run() {
	logFile, err := os.OpenFile(s.cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("open vllm log %s: %v", s.cfg.LogPath, err)
	}
	defer logFile.Close()

	env := os.Environ()
	if s.cfg.HFToken != "" {
		env = append(env, "HF_TOKEN="+s.cfg.HFToken, "HUGGING_FACE_HUB_TOKEN="+s.cfg.HFToken)
	}
	if s.cfg.HFHome != "" {
		env = append(env, "HF_HOME="+s.cfg.HFHome)
	}

	for {
		cmd := exec.Command("vllm", s.cfg.args()...)
		cmd.Env = env
		cmd.Stdout = logFile
		cmd.Stderr = logFile

		s.mu.Lock()
		s.cmd = cmd
		s.mu.Unlock()

		log.Printf("starting vllm serve %s on :%s (log: %s)", s.cfg.Repo, s.cfg.Port, s.cfg.LogPath)
		runErr := cmd.Run()
		log.Printf("vllm serve %s exited: %v - restarting in 10s", s.cfg.Repo, runErr)

		s.mu.Lock()
		s.cmd = nil
		s.mu.Unlock()

		time.Sleep(10 * time.Second)
	}
}

// kill force-stops the currently-running child, if any, so run's loop
// relaunches it. A no-op if it already exited and is mid-backoff (s.cmd
// nil) - the loop is about to relaunch on its own regardless.
func (s *vllmSupervisor) kill() {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	log.Printf("watchdog killing vllm serve %s (unresponsive)", s.cfg.Repo)
	_ = cmd.Process.Kill()
}

// waitVLLMReady polls baseURL's /v1/models until it answers or timeout
// elapses, mirroring mega-deploy.sh's stage_boot polling loop. Uses
// llmBackend.healthCheck rather than a bespoke probe so "ready" means
// exactly what the health monitor will later mean by "healthy".
func waitVLLMReady(baseURL string, timeout time.Duration) error {
	backend := &llmBackend{baseURL: baseURL, client: &http.Client{Timeout: 3 * time.Second}}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if backend.healthCheck() == nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("vllm did not become ready within %s", timeout)
}
