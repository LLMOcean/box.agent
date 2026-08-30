package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// BackendState is the local LLM backend's lifecycle phase, reported to the
// management API so a dashboard can distinguish "still downloading a 9GB+
// model" or "booting vllm serve" from a genuine hang, instead of only
// seeing a bare healthy/unhealthy bool once the backend either comes up or
// times out. See reporter.go's reportStatusLoop for how transitions are
// reported, and main.go/backend.go for who writes each state.
type BackendState string

const (
	BackendStateStarting         BackendState = "starting"          // process launched, not yet probed
	BackendStateDownloadingModel BackendState = "downloading_model" // -deploy-vllm only: hf/huggingface-cli download in progress
	BackendStateBooting          BackendState = "booting"           // vllm serve launched, waiting for /v1/models to answer
	BackendStateReady            BackendState = "ready"             // healthy, serving
	BackendStateUnhealthy        BackendState = "unhealthy"         // was ready, health checks now failing
	BackendStateRestarting       BackendState = "restarting"        // -deploy-vllm only: watchdog killed it, relaunching
	BackendStateCrashed          BackendState = "crashed"           // exited on its own, not via watchdog
)

// GPUStatus is one GPU's live utilization snapshot. Unlike detectGPU
// (benchmark.go), which only reads nvidia-smi's first line for HostInfo's
// existing single-name wire contract (docs/BENCHMARK.md), detectGPUs
// reports every GPU present.
type GPUStatus struct {
	Index              int     `json:"index"`
	Name               string  `json:"name,omitempty"`
	VRAMTotalMB        uint64  `json:"vram_total_mb,omitempty"`
	VRAMUsedMB         uint64  `json:"vram_used_mb,omitempty"`
	UtilizationPercent float64 `json:"utilization_percent,omitempty"`
	TemperatureC       float64 `json:"temperature_c,omitempty"`
}

// HostStatus is a periodic snapshot of the box's own resources - see
// reporter.go's reportStatusLoop, the only caller. disk/uptime/load-average
// are Linux-only (hoststatus_linux.go); they're simply omitted (zero value,
// omitempty) on the Windows/Darwin builds box-agent also ships
// (deploy/install.ps1, deploy/install.sh) - see hoststatus_other.go.
type HostStatus struct {
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	CPUCores int    `json:"cpu_cores,omitempty"`

	LoadAvg1  float64 `json:"load_avg_1,omitempty"`
	LoadAvg5  float64 `json:"load_avg_5,omitempty"`
	LoadAvg15 float64 `json:"load_avg_15,omitempty"`

	MemTotalMB uint64 `json:"mem_total_mb,omitempty"`
	MemFreeMB  uint64 `json:"mem_free_mb,omitempty"`

	DiskTotalMB uint64 `json:"disk_total_mb,omitempty"` // root filesystem
	DiskFreeMB  uint64 `json:"disk_free_mb,omitempty"`

	UptimeSeconds float64 `json:"uptime_seconds,omitempty"`

	GPUs []GPUStatus `json:"gpus,omitempty"`
}

// BackendStatus describes which model/backend is currently being served.
type BackendStatus struct {
	Type          string       `json:"type,omitempty"` // "vllm" | "ollama" | "other"
	Model         string       `json:"model,omitempty"`
	ContextLength int          `json:"context_length,omitempty"`
	Quantization  string       `json:"quantization,omitempty"`
	Healthy       bool         `json:"healthy"`        // no omitempty - false is meaningful
	VLLM          *VLLMMetrics `json:"vllm,omitempty"` // §2 of the telemetry plan - vllm-backend only, regardless of -deploy-vllm
}

// VLLMProcessStatus is box-agent's own supervision view of the vllm serve
// child under -deploy-vllm - process lifecycle (PID, uptime, restarts).
// Distinct from VLLMMetrics (vllmmetrics.go), which is vLLM's own internal
// state scraped from its /metrics endpoint and populated regardless of who
// launched the process. nil unless -deploy-vllm, so the server can tell
// "not vllm-supervised" apart from "vllm-supervised but currently down".
type VLLMProcessStatus struct {
	Running           bool       `json:"running"`
	PID               int        `json:"pid,omitempty"`
	UptimeSeconds     float64    `json:"uptime_seconds,omitempty"`
	RestartCount      int        `json:"restart_count"` // process-lifetime-scoped - resets on box-agent restart
	LastRestartAt     *time.Time `json:"last_restart_at,omitempty"`
	LastRestartReason string     `json:"last_restart_reason,omitempty"` // "crash" | "watchdog"
}

// collectHostStatus gathers everything HostStatus needs. Best-effort
// throughout: any one piece failing (no nvidia-smi, non-Linux disk/uptime)
// just leaves that field at its zero value, omitted by omitempty - matches
// collectHostInfo's (benchmark.go) existing "absence isn't an error"
// philosophy.
func collectHostStatus() HostStatus {
	h := HostStatus{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUCores: runtime.NumCPU()}
	if total, free, err := readProcMeminfo(); err == nil {
		h.MemTotalMB, h.MemFreeMB = total, free
	}
	if total, free, err := diskStats(); err == nil {
		h.DiskTotalMB, h.DiskFreeMB = total, free
	}
	if uptime, err := uptimeSeconds(); err == nil {
		h.UptimeSeconds = uptime
	}
	if a1, a5, a15, err := loadAvg(); err == nil {
		h.LoadAvg1, h.LoadAvg5, h.LoadAvg15 = a1, a5, a15
	}
	if gpus, err := detectGPUs(); err == nil {
		h.GPUs = gpus
	}
	return h
}

// detectGPUs runs nvidia-smi once for every GPU present (unlike detectGPU
// in benchmark.go, which only reads the first line for HostInfo's existing
// single-GPU-name wire contract). LC_ALL=C is forced because some
// driver/locale combinations render decimals with a comma, which silently
// breaks strconv.ParseFloat. Parses row by row so one malformed/N/A row
// (a GPU in an ECC-error or reset state) doesn't fail the whole call - that
// row is skipped rather than aborting collection of the others.
func detectGPUs() ([]GPUStatus, error) {
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.total,memory.used,utilization.gpu,temperature.gpu",
		"--format=csv,noheader,nounits")
	cmd.Env = append(cmd.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	var gpus []GPUStatus
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) < 6 {
			continue // malformed row - skip, don't fail the whole call
		}
		idx, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		g := GPUStatus{Index: idx, Name: fields[1]}
		if v, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
			g.VRAMTotalMB = v
		}
		if v, err := strconv.ParseUint(fields[3], 10, 64); err == nil {
			g.VRAMUsedMB = v
		}
		if v, err := strconv.ParseFloat(fields[4], 64); err == nil {
			g.UtilizationPercent = v
		}
		if v, err := strconv.ParseFloat(fields[5], 64); err == nil {
			g.TemperatureC = v
		}
		gpus = append(gpus, g)
	}
	if len(gpus) == 0 {
		return nil, fmt.Errorf("nvidia-smi returned no parseable GPU rows")
	}
	return gpus, nil
}
