# Telemetry: status and metrics reporting

box-agent proactively reports two things to the management API (`-api-url`,
default `https://api.greenference.com`), on top of the one-shot
`register-model` call and the boolean `report-health` transitions described
elsewhere in this repo: a **lifecycle state + host/backend status snapshot**,
and **aggregated request performance metrics**. Both are HTTP `POST`s,
bearer-token authed the same way every other management-API call already
is — unlike [`BENCHMARK.md`](BENCHMARK.md)'s `benchmark`/`benchmark_result`
frame pair, neither of these goes over the router WebSocket connection, and
neither is router-triggered — box-agent sends both on its own schedule,
regardless of router connection state.

## Why two endpoints, and why not `report-health`

- **`report-status`** is a *merge-patch snapshot* — every field is
  `omitempty` (except `state`/`backend.healthy`/`router_connections`), so a
  zero/unknown value must never overwrite previously-known server-side
  state, the same convention `register-model` already uses.
- **`report-metrics`** is an *append-only time-series event* — the opposite
  semantics: `request_count: 0` is real, meaningful data (the box was idle)
  and is always sent, never omitted.
- **`report-health`** is unchanged and still the authoritative, low-latency,
  debounced transition alert for backend up/down. `report-status`'s
  `backend.healthy` field is a same-signal convenience duplicate (both fed
  by the same internal value, so they can never disagree) for a dashboard
  rendering one snapshot — not a replacement for alerting off the
  transition itself, since a status report can lag a real transition by up
  to `-status-report-interval`.

Both are fire-and-forget: a failed POST is logged and dropped, no local
retry or queue, matching `report-health`'s existing failure posture.

## `POST /llmocean/agent-instances/report-status`

Sent (a) once, immediately, at startup; (b) immediately again on every
`BackendState` transition (see below); (c) as a steady-state heartbeat
every `-status-report-interval` (default `60s`), so a report is never more
than one interval stale even with zero transitions.

```json
{
  "state": "booting",
  "agent_version": "v1.9.4",
  "agent_uptime_seconds": 42.1,
  "host": {
    "os": "linux", "arch": "amd64", "cpu_cores": 16,
    "load_avg_1": 1.2, "load_avg_5": 0.9, "load_avg_15": 0.7,
    "mem_total_mb": 64000, "mem_free_mb": 40000,
    "disk_total_mb": 512000, "disk_free_mb": 128000,
    "uptime_seconds": 3600,
    "gpus": [
      {"index": 0, "name": "NVIDIA GeForce RTX 3090", "vram_total_mb": 24576, "vram_used_mb": 18980, "utilization_percent": 87, "temperature_c": 68}
    ]
  },
  "backend": {
    "type": "vllm", "model": "Qwen/Qwen3.5-9B",
    "context_length": 32000, "quantization": "",
    "healthy": true,
    "vllm": {
      "kv_cache_usage_percent": 0.42, "requests_running": 2, "requests_waiting": 0,
      "prompt_tokens_total": 184230, "generation_tokens_total": 92110,
      "avg_ttft_ms": 210, "avg_time_per_output_token_ms": 18
    }
  },
  "vllm_process": {
    "running": true, "pid": 4821, "uptime_seconds": 301, "restart_count": 0
  },
  "router_connections": 1,
  "router_connections_configured": 1,
  "reported_at": "2026-08-30T12:00:00Z"
}
```

### `state` — backend lifecycle

| Value | Meaning |
|---|---|
| `starting` | Process launched, not yet probed |
| `downloading_model` | `-deploy-vllm` only: Hugging Face download in progress |
| `booting` | `-deploy-vllm` only: `vllm serve` launched, waiting for `/v1/models` |
| `ready` | Healthy, serving |
| `unhealthy` | Was ready, health checks now failing |
| `restarting` | `-deploy-vllm` only: the watchdog killed the child, relaunching |
| `crashed` | Exited on its own — see `vllm_process.last_restart_reason` for the reliable per-process signal; `state` itself only ever moves to `restarting` when box-agent's watchdog acts, since distinguishing "I killed a hung process" from "it had already died" isn't reliable from the health-check loop alone |

For a non-`-deploy-vllm` backend (Ollama, or an already-running server —
e.g. a vast.ai vLLM template, where vLLM is its own supervisor-managed
service, not spawned by box-agent), `state` goes straight from `starting`
to whatever the first health check finds (`ready`, or `unhealthy` until the
backend answers) — `downloading_model`/`booting`/`restarting` never apply.

### `backend.vllm` — vLLM's own metrics

Populated whenever `backend.type == "vllm"`, **independent of
`-deploy-vllm`** — scraped from vLLM's own Prometheus `/metrics` endpoint
at `{-llm-url}/metrics`, which exists regardless of who launched the
process. This is vLLM's own internal accounting (KV cache usage, queue
depth, its own TTFT/throughput), distinct from `report-metrics` below
(box-agent's end-to-end view, including proxying overhead). Best-effort:
omitted entirely if `/metrics` isn't reachable (older vLLM build, or a
non-vLLM backend).

**Not verified against a live instance** at the time this was written — the
field-to-Prometheus-series mapping (`vllm:gpu_cache_usage_perc`,
`vllm:num_requests_running`, etc.) is vLLM's well-documented, but
version-evolving, contract. Confirm with `curl {-llm-url}/metrics` on a
real deployment before relying on these numbers in production.

### `vllm_process` — box-agent's own supervision view

`null` unless `-deploy-vllm`. This is process lifecycle (PID, uptime,
restart count) as box-agent's own `vllmSupervisor` sees it — separate from
`backend.vllm` above, which is vLLM's internal state. `restart_count` is
**process-lifetime-scoped**: it resets to 0 on every box-agent restart,
since there's no persistence layer to carry it across.

## `POST /llmocean/agent-instances/report-metrics`

Sent every `-metrics-report-interval` (default `30s` — also the
aggregation window size), unconditionally, including all-zero idle
windows.

```json
{
  "window_start": "2026-08-30T12:00:00Z",
  "window_seconds": 30.0,
  "request_count": 14,
  "error_count": 0,
  "input_tokens": 5200,
  "output_tokens": 3100,
  "avg_input_tokens_per_request": 371.4,
  "avg_output_tokens_per_request": 221.4,
  "tokens_per_second": 276.7,
  "non_streaming_count": 2,
  "non_streaming_latency_avg_ms": 890,
  "non_streaming_latency_p95_ms": 1600,
  "streaming_count": 12,
  "streaming_ttft_avg_ms": 240,
  "streaming_ttft_p95_ms": 400
}
```

This is box-agent's own **end-to-end** view (router → box-agent → backend
→ back), correlated with request size via
`avg_input_tokens_per_request`/`avg_output_tokens_per_request` — window-level
averages, not a joint per-request distribution. `*_p95_ms` fields are a
**bucketed-histogram approximation**, not an exact percentile: recording a
sample is a lock-free atomic increment into one of eleven fixed buckets
(boundaries at 50/100/200/400/800/1600/3200/6400/12800/25600 ms, plus one
unbounded overflow bucket), the right tradeoff for a concurrent per-request
hot path where an exact sorted-sample percentile would need either a lock
or unbounded memory. `tokens_per_second` is omitted (not sent as `0`) when
`request_count` is `0` — undefined for an idle window, not "0 tok/s".

## Configuration

| Flag | Default | Effect |
|---|---|---|
| `-status-report-interval` | `60s` | Steady-state `report-status` heartbeat cadence |
| `-metrics-report-interval` | `30s` | `report-metrics` window size/flush cadence |
| `-disable-telemetry` | `false` | Skip starting both reporting loops entirely |

## See also

- [`../README.md`](../README.md) — daemon mode, flags, `-deploy-vllm`.
- [`BENCHMARK.md`](BENCHMARK.md) — the router-triggered, on-demand
  `benchmark`/`benchmark_result` frame pair (unrelated transport, similar
  host-diagnostics spirit).
- `hoststatus.go`, `vllmmetrics.go`, `metrics.go`, `reporter.go`,
  `api.go` — implementation.
