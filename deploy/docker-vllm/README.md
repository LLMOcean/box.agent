# box-agent + vLLM, as a vast.ai template

A Docker image that bundles box-agent into Vast.ai's own official `vastai/vllm`
image (https://hub.docker.com/r/vastai/vllm, built from
https://github.com/vast-ai/base-image, `external/vllm`) as one more
supervisor-managed service, so a vast.ai instance can serve a model *and*
register with the LLMOcean router/management API with no SSH step - just
fill in environment variables in the vast.ai template editor.

vLLM itself is untouched - exactly what `vastai/vllm` already runs, still
configured the normal way (`VLLM_MODEL`). box-agent only adds registration
on top, pointed at that same already-running server
(`http://127.0.0.1:18000`, this image's fixed internal vLLM port). It never
uses `-deploy-vllm` here - that would download the model a second time and
launch a competing `vllm serve` fighting the existing one for the same
VRAM. See `../install-supervisord.sh`'s header for the same reasoning
applied over SSH, after the fact, instead of baked into an image.

## Build and publish

```bash
cd deploy/docker-vllm
docker buildx build --platform linux/amd64,linux/arm64 \
  -t <dockerhub-org>/greenference-box-vllm:latest \
  --push .
```

`BASE_TAG` (build arg, default `v0.28.0-cuda-13.0`) pins the underlying
`vastai/vllm` tag - see https://hub.docker.com/r/vastai/vllm/tags for
others (CUDA 12.9 vs 13.0 variants, newer vLLM releases, ...). `BOX_AGENT_VERSION`
(default `latest`) pins the box-agent release baked in.

Built and tested locally against a real `vastai/vllm:v0.28.0-cuda-13.0` base
(no GPU in that environment, so vLLM itself wasn't exercised, but everything
box-agent-side was): box-agent binary present/correct-arch, self-skip when
`PROVIDER`/token/model are unset (`supervisorctl status` shows `EXITED`, not
`FATAL`), and — with `PROVIDER`/`API_TOKEN`/`VLLM_MODEL` set and a
deliberately invalid token — a real round trip to the management API
followed by a genuine crash-restart loop (confirms `autorestart=unexpected`
actually reacts to a real failure, not just a clean exit). Two real bugs
were caught and fixed this way: an unbound `$TARGETARCH` under the classic
(non-buildx) builder, and the `pty` wrapper silently swallowing box-agent's
real exit code (see `box-agent.sh`'s comments). Not yet tested: an actual
vast.ai template launch on real GPU hardware, and a genuinely successful
registration (only the failure path was exercised, deliberately).

## Using it as a vast.ai template

vast.ai's template editor (Config tab) needs a few fields filled in by hand
that `vastai/vllm`'s own official template already carries - a custom
template built from an arbitrary image doesn't inherit them automatically:

**Image Path:Tag**: `<dockerhub-org>/greenference-box-vllm:latest`

**Ports** (TCP): `1111`, `7860`, `8000`, `8265` - matches `vastai/vllm`'s own
port table (Instance Portal, Model UI, vLLM API, Ray Dashboard). SSH (`22`)
is handled by vast.ai automatically.

**Environment Variables** - required for the portal/Caddy wiring (this exact
value is what `vastai/vllm`'s own template sets, confirmed against a live
instance):

```
PORTAL_CONFIG=localhost:1111:11111:/:Instance Portal|localhost:7860:17860:/:Model UI|localhost:8000:18000:/docs:vLLM API|localhost:8265:28265:/:Ray Dashboard
```

Plus the model and box-agent registration variables:

| Variable | Required | Description |
|---|---|---|
| `VLLM_MODEL` | yes | Model for vLLM itself to serve (vastai/vllm's own variable) |
| `PROVIDER` | for registration | e.g. `yourns/your-model` |
| `TOKEN` | one of these two | an existing per-instance agent token |
| `API_TOKEN` | | an IAM/account-level token, to auto-provision a per-instance token on first boot |
| `HF_TOKEN` | if gated | Hugging Face token, for vLLM's own model download (vastai/vllm's own variable, unrelated to box-agent) |
| `BACKEND_MODEL` | no | defaults to `$VLLM_MODEL` - only set separately if it must differ |
| `SUPPORTED_FEATURES`, `CONTEXT_LENGTH`, `IS_PUBLIC`, ... | no | same names/defaults as `../install.sh` - see its `-h` |

Leaving `PROVIDER`/`TOKEN`/`API_TOKEN` unset is fine - box-agent then
self-skips (logs why, exits cleanly, does not crash-loop) and the instance
behaves as a plain `vastai/vllm` template.

## Verifying it worked

From the instance portal or SSH:

```bash
supervisorctl status box-agent
tail -f /var/log/box-agent.log
```

A successful boot logs `registered with API: model=... provider=...`
followed by `connected: wss://...` - the same sequence
`../install-supervisord.sh` produces when run manually over SSH.
