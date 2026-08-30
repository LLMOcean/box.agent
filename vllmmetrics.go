package main

import (
	"bufio"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// VLLMMetrics is scraped from vLLM's own Prometheus /metrics endpoint -
// vLLM's ground-truth internal state, distinct from box-agent's own
// per-connection request tracking (metrics.go), which measures end-to-end
// including box-agent's own proxying overhead. Populated whenever the
// backend is vLLM, independent of -deploy-vllm - vLLM exposes this
// regardless of who launched the process (e.g. the vast.ai vLLM template,
// where vLLM runs as its own supervisor-managed service, not spawned by
// box-agent at all).
//
// Field-to-series mapping is vLLM's well-documented, version-stable
// Prometheus contract, but was NOT verified against a live instance during
// this feature's design (see docs/TELEMETRY.md) - confirm against a real
// `curl {-llm-url}/metrics` before relying on these names in production,
// since vLLM has restructured its metrics across versions before.
type VLLMMetrics struct {
	KVCacheUsagePercent     float64 `json:"kv_cache_usage_percent,omitempty"`       // vllm:gpu_cache_usage_perc, 0-1 gauge
	RequestsRunning         int     `json:"requests_running,omitempty"`             // vllm:num_requests_running
	RequestsWaiting         int     `json:"requests_waiting,omitempty"`             // vllm:num_requests_waiting
	PromptTokensTotal       float64 `json:"prompt_tokens_total,omitempty"`          // vllm:prompt_tokens_total counter
	GenerationTokensTotal   float64 `json:"generation_tokens_total,omitempty"`      // vllm:generation_tokens_total counter
	AvgTTFTMs               float64 `json:"avg_ttft_ms,omitempty"`                  // vllm:time_to_first_token_seconds histogram (sum/count)
	AvgTimePerOutputTokenMs float64 `json:"avg_time_per_output_token_ms,omitempty"` // vllm:time_per_output_token_seconds histogram (sum/count)
}

// scrapeVLLMMetrics does a plain GET {llmURL}/metrics and picks out the
// handful of vllm: series VLLMMetrics needs, via a targeted Prometheus
// text-format line scanner rather than a full client library - the format
// is simple enough for this, matching the codebase's existing preference
// for zero extra dependencies (see go.mod). Best-effort: a missing
// endpoint (older vLLM build, or a non-vLLM backend) or any parse failure
// returns a nil result and the error, so callers can just omit VLLMMetrics
// from their report rather than fail it.
func scrapeVLLMMetrics(llmURL string) (*VLLMMetrics, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimSuffix(llmURL, "/")+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vllm metrics endpoint returned status %d", resp.StatusCode)
	}

	series := make(map[string]float64)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// "metric_name{label="v",...} value" or "metric_name value" - the
		// name is everything up to the first "{" or space, whichever comes
		// first; the value is always the last whitespace-separated field.
		name := line
		if idx := strings.IndexAny(line, "{ "); idx != -1 {
			name = line[:idx]
		}
		if !strings.HasPrefix(name, "vllm:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		// Sum across label variants (e.g. if vLLM ever reports per-model
		// rows) - box-agent talks to one model on one -llm-url, so in
		// practice this is almost always a single matching row.
		series[name] += value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(series) == 0 {
		return nil, fmt.Errorf("no vllm: series found in /metrics response")
	}

	m := &VLLMMetrics{
		KVCacheUsagePercent:   series["vllm:gpu_cache_usage_perc"],
		RequestsRunning:       int(series["vllm:num_requests_running"]),
		RequestsWaiting:       int(series["vllm:num_requests_waiting"]),
		PromptTokensTotal:     series["vllm:prompt_tokens_total"],
		GenerationTokensTotal: series["vllm:generation_tokens_total"],
	}
	if count := series["vllm:time_to_first_token_seconds_count"]; count > 0 {
		m.AvgTTFTMs = (series["vllm:time_to_first_token_seconds_sum"] / count) * 1000
	}
	if count := series["vllm:time_per_output_token_seconds_count"]; count > 0 {
		m.AvgTimePerOutputTokenMs = (series["vllm:time_per_output_token_seconds_sum"] / count) * 1000
	}
	return m, nil
}
