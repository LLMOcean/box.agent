# Telemetry API implementation spec (management API side)

**Audience:** the management API team (the service behind `-api-url`,
default `https://api.greenference.com` — a separate codebase from
box.agent, not present in this repo). This document specifies the two new
endpoints box.agent now calls, and the database tables needed to receive
them. box.agent's own side (what it sends, when, and why) is fully
implemented and documented in [`TELEMETRY.md`](TELEMETRY.md) — this
document is the server-side counterpart: what to build to receive it.

Existing conventions this follows (inferred from `api.go` and the wire
responses already observed from the live API): routes under
`/llmocean/agent-instances/...`, bearer-token auth resolving to a row in
what's referred to here as `llmocean_agent_instances` (the table backing
`AgentInstancesService`, per `api.go`'s existing comments —
`llmocean_provider_id`, `instance_name`, `is_active`, `status` are already
known fields on it), and a `llmocean_` table-name prefix. Adjust naming to
match whatever the actual schema uses if it differs from these inferences.

## Why two endpoints, and how they differ

| | `report-status` | `report-metrics` |
|---|---|---|
| Shape | State **snapshot** | Time-series **event** |
| Write pattern | **Upsert** — one current row per instance | **Insert** — one new row per window, forever |
| Zero/missing field | Means "unknown, don't touch" — never overwrite existing data with it | Means real data (e.g. 0 requests = idle) — always store as sent |
| Frequency | On every lifecycle transition (near-instant) + a steady heartbeat (default 60s) | Every fixed window (default 30s), unconditionally, including empty windows |

This distinction should drive the schema: `report-status` wants an
upsert-in-place table (plus, optionally, a small append-only transition-log
table for history — see below); `report-metrics` wants a pure append-only
table with a retention/rollup strategy, since it grows forever at a fixed
rate per active instance.

Both endpoints are called by box.agent on a fire-and-forget basis — it
logs and drops a failed call, no retry. A dropped report is simply gone;
nothing on the client side re-sends it. Treat both as best-effort ingestion
(a brief outage loses that report, not more).

---

## Endpoint 1: `POST /llmocean/agent-instances/report-status`

**Auth:** same bearer-token scheme as `report-health`/`register-model` —
resolve the token to its `llmocean_agent_instances` row the same way.

**Response:** `200`/`201`/`204` all treated as success by box.agent (no
response body is parsed) — `204 No Content` is the natural choice. Any
other status is logged client-side as a failure and simply dropped (no
retry), so a `422`/`500` here has no user-visible effect beyond a gap in
that instance's telemetry.

### Request body

```json
{
  "state": "booting",
  "agent_version": "v1.9.4",
  "agent_uptime_seconds": 42.1,
  "host": {
    "os": "linux",
    "arch": "amd64",
    "cpu_cores": 16,
    "load_avg_1": 1.2,
    "load_avg_5": 0.9,
    "load_avg_15": 0.7,
    "mem_total_mb": 64000,
    "mem_free_mb": 40000,
    "disk_total_mb": 512000,
    "disk_free_mb": 128000,
    "uptime_seconds": 3600,
    "gpus": [
      {
        "index": 0,
        "name": "NVIDIA GeForce RTX 3090",
        "vram_total_mb": 24576,
        "vram_used_mb": 18980,
        "utilization_percent": 87,
        "temperature_c": 68
      }
    ]
  },
  "backend": {
    "type": "vllm",
    "model": "Qwen/Qwen3.5-9B",
    "context_length": 32000,
    "quantization": "",
    "healthy": true,
    "vllm": {
      "kv_cache_usage_percent": 0.42,
      "requests_running": 2,
      "requests_waiting": 0,
      "prompt_tokens_total": 184230,
      "generation_tokens_total": 92110,
      "avg_ttft_ms": 210,
      "avg_time_per_output_token_ms": 18
    }
  },
  "vllm_process": {
    "running": true,
    "pid": 4821,
    "uptime_seconds": 301,
    "restart_count": 0,
    "last_restart_at": null,
    "last_restart_reason": null
  },
  "router_connections": 1,
  "router_connections_configured": 1,
  "reported_at": "2026-08-30T12:00:00Z"
}
```

### Field reference

| Field | Type | Always present? | Notes |
|---|---|---|---|
| `state` | string enum | always | `starting` \| `downloading_model` \| `booting` \| `ready` \| `unhealthy` \| `restarting` \| `crashed` — see state table below. **Never omitted, never null.** |
| `agent_version` | string | omitted if unknown | box-agent's own build version |
| `agent_uptime_seconds` | number | omitted if unknown | box-agent process uptime, not the host's |
| `host.*` | object | fields individually omitted if uncollectable | see host table below |
| `host.gpus` | array | omitted if no GPU / `nvidia-smi` unavailable | 0 or more entries; **can shrink or grow between reports** in theory (rare) — treat as authoritative-for-this-report, not append-only |
| `backend.type` | string enum | omitted only if genuinely undetected | `vllm` \| `ollama` \| `other` |
| `backend.model` | string | usually present | |
| `backend.context_length` | int | omitted if unknown | |
| `backend.quantization` | string | omitted if unknown/empty | |
| `backend.healthy` | bool | **always present, never omitted** | `false` is meaningful — do not treat absence and `false` the same |
| `backend.vllm` | object, nullable | present only when `backend.type == "vllm"` **and** vLLM's own `/metrics` was reachable | best-effort scrape from vLLM's own Prometheus endpoint — see caveat below |
| `vllm_process` | object, nullable | **`null` unless the instance is running `-deploy-vllm`** | box-agent's own process supervision view, not vLLM's internal state |
| `vllm_process.restart_count` | int | | **process-lifetime-scoped** — resets to 0 whenever box-agent itself restarts. Do not treat a drop to 0 as "no more crashes have ever happened" |
| `router_connections` | int | **always present, never omitted** | `0` is the alarm signal (no live router connection) |
| `router_connections_configured` | int | always present | how many the instance was configured to open (`-connections` flag, normally `1`) |
| `reported_at` | ISO 8601 timestamp | always | box-agent's own wall clock — **not NTP-synced**, treat as advisory/display-only, not for strict cross-instance time-bucketing |

**Caveat on `backend.vllm`'s field names:** these map to vLLM's own
Prometheus metric names (`vllm:gpu_cache_usage_perc`,
`vllm:num_requests_running`, etc.), which have shifted across vLLM
versions historically. If a future box.agent release changes what it
sends here, treat unknown/renamed fields as additive — don't hard-fail on
an unrecognized key.

### `state` values

| Value | Meaning | `vllm_process` expected? |
|---|---|---|
| `starting` | Process launched, not yet probed | maybe (if `-deploy-vllm`, before download starts) |
| `downloading_model` | Hugging Face download in progress | yes, not yet running |
| `booting` | `vllm serve` launched, waiting for readiness | yes, `running: true` |
| `ready` | Healthy, serving | yes if `-deploy-vllm` |
| `unhealthy` | Was ready, health checks now failing | yes if `-deploy-vllm` |
| `restarting` | Watchdog killed a hung process, relaunching | yes if `-deploy-vllm` |
| `crashed` | Currently unused by box-agent itself (see `TELEMETRY.md`) — reserved for future use or server-side derivation from `vllm_process.last_restart_reason == "crash"` | — |

`downloading_model`/`booting`/`restarting` only ever appear for instances
running `-deploy-vllm`; an instance pointed at an already-running backend
(e.g. Ollama, or a pre-existing vLLM service it didn't launch) goes
straight from `starting` to `ready`/`unhealthy`.

### Server-side behavior

1. Resolve the bearer token to an agent instance (same as `report-health`/`register-model`).
2. **Upsert** into a single "current status" row per instance — every field
   present in the request overwrites the stored value; every field absent
   (`omitempty` on the client) is **left untouched**, not nulled out. This
   is the one behavior most worth getting right: a partial report (e.g.
   `host.gpus` omitted because `nvidia-smi` had a transient hiccup) must
   not erase previously-known GPU data.
3. **Recommended, not required:** if `state` differs from the
   previously-stored value for this instance, also insert a row into an
   append-only transition-log table (see schema below) — this is what
   makes a "when did this instance start downloading / go unhealthy /
   recover" timeline view possible without scanning the (frequently
   overwritten) current-status table's history.

---

## Endpoint 2: `POST /llmocean/agent-instances/report-metrics`

**Auth:** identical bearer-token scheme.

**Response:** identical status-code contract to `report-status`.

### Request body

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

### Field reference

| Field | Type | Always present? | Notes |
|---|---|---|---|
| `window_start` | ISO 8601 timestamp | always | box-agent's own clock, advisory only |
| `window_seconds` | number | always | nominally equals `-metrics-report-interval` (default 30s), but the *actual* elapsed time since the previous flush — use this, not the configured interval, for rate calculations |
| `request_count` | int | **always, never omitted** | `0` is real data (idle window) |
| `error_count` | int | always | requests that resulted in an `error` frame to the router |
| `input_tokens` / `output_tokens` | int | always | window totals |
| `avg_input_tokens_per_request` / `avg_output_tokens_per_request` | number | omitted when `request_count == 0` | |
| `tokens_per_second` | number | omitted when `request_count == 0` | **undefined for an idle window, not `0`** — don't default a missing value to zero in a rate chart, treat it as "no data point" |
| `non_streaming_count` / `streaming_count` | int | always | |
| `non_streaming_latency_avg_ms` / `streaming_ttft_avg_ms` | number | omitted when the corresponding count is 0 | |
| `non_streaming_latency_p95_ms` / `streaming_ttft_p95_ms` | number | omitted when the corresponding count is 0 | **bucketed-histogram approximation, not an exact percentile** — resolution-limited to box-agent's fixed bucket boundaries (50/100/200/400/800/1600/3200/6400/12800/25600 ms). Fine for dashboards/alerting; don't present as exact |

### Server-side behavior

Pure **insert** — one new row per call, no upsert, no dedup logic needed
(box-agent never re-sends a dropped window). This table grows at a fixed
rate per active instance: at the default 30s window, that's ~2,880
rows/day/instance. **A retention/rollup strategy is a required design
decision here, not optional** — options, roughly in order of effort:

1. A scheduled job that deletes raw rows older than N days (simplest;
   loses fine-grained history).
2. A scheduled job that rolls raw rows into hourly/daily aggregates in a
   separate summary table, then prunes the raw ones (keeps long-term
   trends, loses per-30s granularity beyond the retention window).
3. If the DB has time-series-native support (e.g. a Postgres +
   TimescaleDB setup, or a dedicated metrics store already in use
   elsewhere in the stack) — use that instead of a plain table, and treat
   everything below as the logical schema to map onto it.

This isn't a decision box.agent's own design can make for you — flagging
it explicitly so it's a conscious call, not something noticed after the
table is already large.

---

## Database schema

Written as portable-ish SQL (PostgreSQL-flavored — swap `JSONB` for `JSON`
on MySQL, and adjust the timestamp/serial types to match whatever the rest
of the schema already uses). Table names follow the `llmocean_` prefix
convention already visible in `llmocean_provider_models`/
`llmocean_agent_instances` — adjust if the actual schema differs.

### `llmocean_agent_instance_status` — current snapshot (upsert target)

One row per agent instance, always overwritten by `report-status`.

```sql
CREATE TABLE llmocean_agent_instance_status (
    agent_instance_id    BIGINT PRIMARY KEY REFERENCES llmocean_agent_instances(id) ON DELETE CASCADE,

    state                 TEXT NOT NULL,  -- enum, see state table above
    agent_version         TEXT,
    agent_uptime_seconds  DOUBLE PRECISION,

    -- host.*
    host_os              TEXT,
    host_arch            TEXT,
    host_cpu_cores       INT,
    host_load_avg_1      DOUBLE PRECISION,
    host_load_avg_5      DOUBLE PRECISION,
    host_load_avg_15     DOUBLE PRECISION,
    host_mem_total_mb    BIGINT,
    host_mem_free_mb     BIGINT,
    host_disk_total_mb   BIGINT,
    host_disk_free_mb    BIGINT,
    host_uptime_seconds  DOUBLE PRECISION,
    host_gpus            JSONB,  -- array of {index,name,vram_total_mb,vram_used_mb,utilization_percent,temperature_c} - see note below

    -- backend.*
    backend_type            TEXT,
    backend_model           TEXT,
    backend_context_length  INT,
    backend_quantization    TEXT,
    backend_healthy         BOOLEAN NOT NULL,
    backend_vllm_metrics    JSONB,  -- {kv_cache_usage_percent, requests_running, requests_waiting, prompt_tokens_total, generation_tokens_total, avg_ttft_ms, avg_time_per_output_token_ms}

    -- vllm_process.* (all null unless -deploy-vllm)
    vllm_process_running             BOOLEAN,
    vllm_process_pid                 INT,
    vllm_process_uptime_seconds      DOUBLE PRECISION,
    vllm_process_restart_count       INT,
    vllm_process_last_restart_at     TIMESTAMPTZ,
    vllm_process_last_restart_reason TEXT,

    router_connections             INT NOT NULL,
    router_connections_configured  INT,

    reported_at  TIMESTAMPTZ NOT NULL,               -- box-agent's own clock, advisory
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()   -- server-side receipt time, authoritative for "how stale is this row"
);

CREATE INDEX idx_agent_instance_status_state ON llmocean_agent_instance_status (state);
CREATE INDEX idx_agent_instance_status_updated_at ON llmocean_agent_instance_status (updated_at);
```

**Why `host_gpus`/`backend_vllm_metrics` as JSON rather than normalized
child tables:** both are small, always-overwritten-together-with-the-parent-
row blobs on a table already being upserted every 5-60s per instance.
Normalizing `host_gpus` into its own table would mean a delete-then-reinsert
per report (GPU count is expected to be static per instance, but the
churn cost is the same either way) for no real query benefit — nobody is
expected to `JOIN` across GPUs by index across instances. If fleet-wide GPU
aggregate queries turn out to matter later (e.g. "average utilization
across every RTX 3090"), that's a reason to revisit, not a reason to
normalize upfront.

**A separate `updated_at` (server receipt time) vs `reported_at` (box-agent's
own clock) is worth keeping distinct** — `reported_at` can drift/be wrong on
a box with a bad clock; `updated_at` is what "is this instance's data
stale" checks should actually use.

### `llmocean_agent_instance_state_events` — transition history (recommended)

Append-only, one row per **actual** `state` change (server detects the
transition by comparing the incoming `state` to what's currently stored in
`llmocean_agent_instance_status` before upserting) — not one row per
`report-status` call, which would just duplicate the heartbeat cadence.

```sql
CREATE TABLE llmocean_agent_instance_state_events (
    id                  BIGSERIAL PRIMARY KEY,
    agent_instance_id   BIGINT NOT NULL REFERENCES llmocean_agent_instances(id) ON DELETE CASCADE,
    from_state           TEXT,  -- null for the very first event
    to_state              TEXT NOT NULL,
    occurred_at            TIMESTAMPTZ NOT NULL DEFAULT now()  -- server receipt time
);

CREATE INDEX idx_agent_instance_state_events_instance ON llmocean_agent_instance_state_events (agent_instance_id, occurred_at);
```

This table is what makes "show me this instance's boot timeline" or
"alert if any instance has been stuck in `downloading_model` for over an
hour" possible without diffing heartbeat snapshots.

### `llmocean_agent_instance_metrics` — time-series (append-only)

```sql
CREATE TABLE llmocean_agent_instance_metrics (
    id                  BIGSERIAL PRIMARY KEY,
    agent_instance_id   BIGINT NOT NULL REFERENCES llmocean_agent_instances(id) ON DELETE CASCADE,

    window_start    TIMESTAMPTZ NOT NULL,
    window_seconds  DOUBLE PRECISION NOT NULL,

    request_count  INT NOT NULL,
    error_count    INT NOT NULL,
    input_tokens   BIGINT NOT NULL,
    output_tokens  BIGINT NOT NULL,
    avg_input_tokens_per_request   DOUBLE PRECISION,
    avg_output_tokens_per_request  DOUBLE PRECISION,
    tokens_per_second              DOUBLE PRECISION,

    non_streaming_count          INT NOT NULL,
    non_streaming_latency_avg_ms DOUBLE PRECISION,
    non_streaming_latency_p95_ms DOUBLE PRECISION,

    streaming_count        INT NOT NULL,
    streaming_ttft_avg_ms  DOUBLE PRECISION,
    streaming_ttft_p95_ms  DOUBLE PRECISION,

    received_at  TIMESTAMPTZ NOT NULL DEFAULT now()  -- server receipt time
);

CREATE INDEX idx_agent_instance_metrics_instance_window ON llmocean_agent_instance_metrics (agent_instance_id, window_start);
```

Add a retention/rollup mechanism per the discussion above before this goes
to production with any real fleet size — it is the one table here with
unbounded growth.

---

## Anticipated query patterns (to sanity-check the indexes above)

- "Current status of instance X" → `SELECT * FROM llmocean_agent_instance_status WHERE agent_instance_id = ?` (PK lookup, no index needed beyond the PK).
- "All instances currently unhealthy/crashed" → `WHERE state IN ('unhealthy','crashed')` (covered by `idx_agent_instance_status_state`).
- "Instances that haven't reported in over 5 minutes" (stale/dead detection — box-agent's process can die without ever sending a final report) → `WHERE updated_at < now() - interval '5 minutes'` (covered by `idx_agent_instance_status_updated_at`).
- "Timeline for instance X" → `SELECT * FROM llmocean_agent_instance_state_events WHERE agent_instance_id = ? ORDER BY occurred_at` (covered).
- "Request throughput for instance X over the last 24h" → `SELECT * FROM llmocean_agent_instance_metrics WHERE agent_instance_id = ? AND window_start > now() - interval '24 hours' ORDER BY window_start` (covered).

## Rollout notes

- Purely additive — no changes needed to `report-health`, `register-model`,
  or `authenticate`. Existing box-agent binaries that predate this feature
  simply never call these two new endpoints; nothing breaks.
- Since box.agent treats a failed report as fire-and-forget (log and drop,
  no retry), the endpoints can be deployed and start receiving traffic at
  any time relative to a box.agent fleet rollout — there's no ordering
  requirement, just a gap in telemetry for any instance running an older
  binary or hitting these routes before they exist (a `404`, logged and
  dropped client-side, no crash).
- Recommend accepting and ignoring unrecognized fields in both request
  bodies (rather than rejecting on unknown keys) — this repo's own
  `vllm_metrics` field names are explicitly flagged as not yet verified
  against every vLLM version, and are the most likely field set to change
  in a future box.agent release.
