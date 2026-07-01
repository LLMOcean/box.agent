# Benchmark Extension — `box.agent`-local

This is a `box.agent`-local extension to the router's wire protocol (see
[`AGENT_PROTOCOL.md`](AGENT_PROTOCOL.md)) — it is not part of the upstream
`router.agent` spec. It lets a router (or any client speaking the same
frame format) ask a running agent instance for a point-in-time read on the
host it's running on and one live LLM timing sample, without going through
a full chat request.

## Request

```json
{
  "type": "benchmark",
  "request_id": "b1",
  "model": "llama-3.1-70b-instruct",
  "prompt": ""
}
```

| Field | Description |
|---|---|
| `model` | Model to run the timing sample against, passed straight through to the configured LLM backend (subject to `-backend-model` override, same as `chat`). |
| `prompt` | Prompt to send for the timing sample. If empty, the agent uses a small built-in default. |

## Response

```json
{
  "type": "benchmark_result",
  "request_id": "b1",
  "benchmark": {
    "ran_at": "2026-07-01T12:00:00Z",
    "host": {
      "os": "linux",
      "arch": "amd64",
      "cpu_cores": 16,
      "mem_total_mb": 32000,
      "mem_free_mb": 21000,
      "gpu": "NVIDIA GeForce RTX 4090"
    },
    "llm": {
      "model": "llama-3.1-70b-instruct",
      "prompt": "Write a two-sentence summary of why the sky is blue.",
      "ttft_ms": 120,
      "total_ms": 900,
      "output_tokens": 42,
      "tokens_per_sec": 46.7
    }
  }
}
```

`host` is cheap-to-read static info about the machine (CPU/mem from
`/proc/meminfo` on Linux, GPU name via `nvidia-smi` if present — both
best-effort, absence is not an error). `llm` is one real streaming
completion against the configured backend, timed for time-to-first-token,
total wall time, and resulting output tokens/sec.

There is no streaming variant of `benchmark` — always exactly one
`benchmark_result` frame in reply, even though the LLM call underneath it
streams.

## Errors

If the LLM call itself fails, `llm.error` is set on the nested
`LLMBenchmark` object and the timing fields are left at their zero values —
the agent still responds with `benchmark_result` (not a top-level `error`
frame), since host info was successfully collected regardless.
