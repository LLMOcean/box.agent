package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// HostInfo captures cheap-to-read facts about the machine box.agent runs on
// -- not a stress test, just what the OS already knows.
type HostInfo struct {
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	CPUCores   int    `json:"cpu_cores"`
	MemTotalMB uint64 `json:"mem_total_mb,omitempty"`
	MemFreeMB  uint64 `json:"mem_free_mb,omitempty"`
	GPU        string `json:"gpu,omitempty"`
}

func collectHostInfo() HostInfo {
	h := HostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUCores: runtime.NumCPU()}
	if total, free, err := readProcMeminfo(); err == nil {
		h.MemTotalMB, h.MemFreeMB = total, free
	}
	if gpu, err := detectGPU(); err == nil {
		h.GPU = gpu
	}
	return h
}

// readProcMeminfo reads /proc/meminfo (Linux only -- box.agent's deployment
// target per the systemd unit in deploy/). Absence of the file just means no
// memory figures are reported, not a benchmark failure.
func readProcMeminfo() (totalMB, freeMB uint64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	var totalKB, availKB uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			totalKB, _ = strconv.ParseUint(fields[1], 10, 64)
		case "MemAvailable:":
			availKB, _ = strconv.ParseUint(fields[1], 10, 64)
		}
	}
	return totalKB / 1024, availKB / 1024, nil
}

// detectGPU shells out to nvidia-smi if present. No GPU / no nvidia-smi is
// the common case (CPU or non-NVIDIA boxes) and is not an error worth
// surfacing -- the caller just omits the field.
func detectGPU() (string, error) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader").Output()
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if name == "" {
		return "", fmt.Errorf("nvidia-smi returned no GPU name")
	}
	return name, nil
}

// LLMBenchmark measures one real streaming chat completion against the
// configured local LLM backend: time to first token, total wall time, and
// resulting throughput -- the numbers that actually predict how this box
// will perform under router traffic.
type LLMBenchmark struct {
	Model        string  `json:"model"`
	Prompt       string  `json:"prompt"`
	TTFTMs       float64 `json:"ttft_ms,omitempty"`
	TotalMs      float64 `json:"total_ms"`
	OutputTokens int     `json:"output_tokens"`
	TokensPerSec float64 `json:"tokens_per_sec,omitempty"`
	Error        string  `json:"error,omitempty"`
}

const defaultBenchmarkPrompt = "Write a two-sentence summary of why the sky is blue."

func runLLMBenchmark(backend *llmBackend, model, prompt string) LLMBenchmark {
	if prompt == "" {
		prompt = defaultBenchmarkPrompt
	}
	result := LLMBenchmark{Model: model, Prompt: prompt}

	start := time.Now()
	var firstTokenAt time.Time
	msgs := []openaiMessage{{Role: "user", Content: prompt}}

	err := backend.stream(model, msgs, 0, nil, nil, SamplingParams{},
		func(chunk string) {
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
		},
		func(usage *Usage, finishReason string, toolCalls []ToolCall) {
			total := time.Since(start)
			result.TotalMs = float64(total.Milliseconds())
			if !firstTokenAt.IsZero() {
				result.TTFTMs = float64(firstTokenAt.Sub(start).Milliseconds())
			}
			if usage != nil {
				result.OutputTokens = usage.OutputTokens
				if total.Seconds() > 0 {
					result.TokensPerSec = float64(usage.OutputTokens) / total.Seconds()
				}
			}
		})
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// BenchmarkResult is the full report for one benchmark run: static host
// facts plus one live LLM timing sample.
type BenchmarkResult struct {
	RanAt time.Time    `json:"ran_at"`
	Host  HostInfo     `json:"host"`
	LLM   LLMBenchmark `json:"llm"`
}

func runBenchmark(backend *llmBackend, model, prompt string) BenchmarkResult {
	return BenchmarkResult{
		RanAt: time.Now().UTC(),
		Host:  collectHostInfo(),
		LLM:   runLLMBenchmark(backend, model, prompt),
	}
}
