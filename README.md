# FlowEdge

**A private, single-binary multimodal workflow engine for one consumer GPU.**

Chain local LLM, text-to-image, video, ASR, TTS and RAG nodes on one canvas — then run the whole pipeline with one click. Everything stays on your machine. No Docker, no Node, no Python, no cloud account.

```bash
# Download FlowEdge.exe, double-click, open http://127.0.0.1:8080
```

![FlowEdge in action — 30-second generated film with stereo audio, produced entirely on one RTX 3090](docs/demo/flowedge-film-demo.gif)

*The clip above (dialogue, ambience, lip movement) was generated locally with MiniMax-H3 on a single 24 GB GPU — orchestrated as a FlowEdge workflow.*

---

## Why another workflow tool

| Tool | What it does | What FlowEdge adds |
|---|---|---|
| **ComfyUI** (137k★) | Node graph for diffusion models | Multimodal orchestration *around* it: LLM / ASR / TTS / RAG / video assembly in the same DAG, plus **VRAM arbitration** so ComfyUI and your LLM can share one 24 GB card |
| **n8n / Dify / Langflow** | LLM workflow platforms | Runs **local models** end-to-end (not just API calls), ships as **one exe**, and treats *generated media quality* as a first-class, machine-checkable concern |
| **Ollama / LocalAI** | Local model servers | Not a chat box: a production engine with debugger, breakpoints, lineage, versions and quality gates |

FlowEdge does not replace ComfyUI — it **orchestrates** it (and Ollama, ffmpeg, Whisper, Kokoro) the way a build system orchestrates compilers.

## Three things that are hard to copy

These are not design decisions. They grew out of real production failures on this machine, and the numbers below are measured, not estimated.

### 1. VRAM arbitration on a single card

24 GB cannot hold a diffusion model (17 GB), an LLM (9 GB) and Whisper (3 GB) at once. FlowEdge arbitrates:

- Enter one side → others are **really unloaded** (`keep_alive:0` / ComfyUI `POST /free`), not just marked free
- Poll `vram_free` until it actually returns above 16 GB before continuing (CUDA release is async)
- Keep ComfyUI weights **resident** within a batch: 5 images at 768² take **199 s** when reloading per image vs **29 s** resident — a measured 6.9×
- Speech services default to CPU, leaving the whole card for image and LLM work

Result: a text-to-image + vision pipeline peaks at **15.0 GB** on a 3090 *while UE5 is open* (was 24.0 GB before arbitration).

### 2. Scene consistency that is enforced, not described

Multi-segment video drifts — shot 3 quietly becomes a different room while duration and audio stay perfect. FlowEdge compiles a 3×3 physical-facts grid into a single `SCENE LOCK` sentence, injects it **byte-identical** into every segment, and **fails the run** if any segment's lock drifts (`scene_lock_missing` / `scene_lock_drift`). Consistency by verification, not by adjectives.

### 3. Quality gates as nodes

`verify-media.py` catches what duration checks cannot: a 0.04 s crossfade once silently dropped an entire segment while every timestamp looked fine. FlowEdge ships this as a **`quality_gate` node** — frame-exact counting (`-count_frames`), A/V drift, silence gaps, loudness, speech detection — wired into the graph, failing the run *before* assembly instead of after delivery.

Plus: H3 dual-worktree isolation (torch 2.14 vs 2.6 coexisting on one machine via exclusive port arbitration) — the kind of knowledge that only comes from debugging, never from reading source.

## Interop: four ways in, none of them a connector catalogue

FlowEdge does not chase n8n's 400 connectors. It borrows other ecosystems through protocols instead:

| Direction | What it gives you |
|---|---|
| **OpenAI-compatible endpoints** | `POST /v1/images/generations`, `/v1/audio/transcriptions`, `/v1/audio/speech`, `/v1/chat/completions` (SSE streaming), `GET /v1/models`. Point Open WebUI / SillyTavern / Chatbox / LangChain at `http://127.0.0.1:8080/v1` and your local GPU becomes their backend — zero client changes. |
| **MCP server** | `FlowEdge.exe --mcp` speaks stdio JSON-RPC. Claude Desktop / Cursor get `flowedge_run_workflow`, `run_status`, `estimate`, `generate_image` as tools. One integration point instead of a hundred connectors — MCP clients are the side that is growing. |
| **ComfyUI workflow import** | The `comfy_workflow` node runs any ComfyUI **Export (API)** JSON as-is. The community has tens of thousands of workflow files; this eats them without writing a node per feature. UI format (nodes/links) is rejected with the exact menu item to use instead — guessing a conversion would produce a graph that looks runnable and is wired wrong. |
| **Runtime plugins** | `data/plugins/<name>/plugin.json` — an `http` plugin (written in any language) or a `script` plugin (interpreted in-process by yaegi, sandboxed to a stdlib allowlist). Adding a node no longer means recompiling a 32 MB exe. |

## Prediction before execution

Every heavy node is sampled for VRAM peak (`nvidia-smi`, 1 s interval, only during the node's own execution window — no idle polling). Those measurements accumulate into `data/vram_profiles.json`, keyed by model × resolution × frames × steps.

`POST /api/estimate` then answers, *before* you commit:

> peak 21.8 GB (range 18.5–25.1), wall 26–41 min, confidence: medium (5 samples)

Three rules keep it honest: no extrapolation across parameter ranges (it says "extrapolated" and only gives an upper bound), confidence drops when samples < 3, and peaks that came from a model-size estimate rather than a sample are marked `peak_underestimated`. `fits` is judged on the upper bound, not the expected value.

## Configuration, not code

| File | Controls |
|---|---|
| `data/models.json` | Logical role → weight files + default params (steps/cfg/size/frames) + precision ladder |
| `data/instances.json` | Model → ComfyUI instance routing; `exclusive_group` serialises instances that cannot coexist on one card |
| `data/plugins/` | Runtime nodes |

Before this, those weight names were hardcoded in 8 places — swapping a checkpoint meant editing Go and rebuilding. It also meant that on any machine without those exact filenames, the first run failed with ComfyUI's `400 prompt_outputs_failed_validation`.

## Features

**Orchestration**
- Visual DAG canvas (React Flow v12): drag, snap, connect with validation
- Layered parallel execution with diamond joins, `join: any`, `continue_on_error`
- 30 node types: LLM / T2I / img2img / inpaint / upscale / T2V / H3 audio-video / shot list / ASR / TTS / vision / select / RAG / code / HTTP / transform / aggregate / loop / sub-workflow / **quality_gate** / **comfy_workflow** (imported ComfyUI graph) / runtime plugins
- Single-step debugger + breakpoints; resume from any node; branch-aware resume reuses successful outputs
- Variable system `{{nodeId.path}}` with array indexing
- Webhook + cron triggers; versioning with rollback; AES-256-GCM credential store
- Artifact lineage: every image / audio / video traces back to workflow → run → node → inputs

**Multimodal (all local)**
- Ollama LLM (streaming + tool calling), ComfyUI T2I with custom workflow templates, Wan 2.1 / MiniMax-H3 video (H3 generates **joint video + stereo audio**), Whisper ASR, Kokoro TTS (Windows SAPI zero-dependency fallback), BM25+vector hybrid RAG with MMR

**Engineering discipline**
- Explicit failure over silent success: unconfigured services fail loudly, never fabricate results
- Save-time validation: cycles, missing prompts and missing inputs are rejected before they enter the database
- Run-time preflight: missing dependencies fail at the entry point with *what is missing and how to start it*
- 300+ tests, structured error codes, persistent job queue with crash recovery, preflight against `/object_info` so wrong weight names fail before submit instead of after

## Quick start

**Windows (recommended — nothing to install)**

1. Download `FlowEdge.exe` (~32 MB) from [Releases](../../releases) — frontend, database and voice-service scripts are embedded
2. Double-click. Browser opens at `http://127.0.0.1:8080`
3. Install optional backends as needed (Ollama / ComfyUI / ffmpeg / Python) — the **Environment** panel tells you exactly what is missing and how to fix it. The engine runs without any of them; only the corresponding nodes will report.

**From source**

```bash
# backend (Go 1.25+)
cd backend && go run main.go

# frontend (Node 18+)
cd frontend && npm install && npm run dev
```

**Build the single exe**

```bash
python scripts/build-windows.py   # → dist/FlowEdge.exe
```

**Linux / macOS (one-command local run)**

`scripts/run-local.sh` builds the binary if needed (cross-compile-free, `CGO_ENABLED=0`),
starts it in the background, polls `/api/health`, and prints the client URL.

```bash
./scripts/run-local.sh            # build-if-needed + start + health-check → http://127.0.0.1:8080
./scripts/run-local.sh --build    # force a rebuild
./scripts/run-local.sh --stop     # stop the background engine
./scripts/run-local.sh --logs     # follow the engine log
```

Build a Linux/amd64 binary directly (for container or remote deploy):

```bash
python scripts/build-windows.py --goos linux --goarch amd64 --output flowedge   # → dist/flowedge
```

## Documentation

| Document | Contents |
|---|---|
| [Multimodal film playbook](docs/多模态出片手册.md) (zh) | Route selection / control gates / prompt templates / measured parameters / acceptance / troubleshooting — one page each |
| [中文 README](docs/zh/README.md) | Full Chinese documentation |
| `AGENTS.md` | Operational discipline: every environment pitfall this project has hit, with evidence |

## Tech stack

Go 1.25 + Gin + GORM + Gorilla WebSocket · React 18 + TypeScript + @xyflow/react + Tailwind · SQLite (zero-dependency; Postgres switchable) · Ollama / ComfyUI / ffmpeg / Whisper / Kokoro

## Security posture

- Listens on `127.0.0.1` by default; external exposure requires explicit `FLOWEDGE_HOST=0.0.0.0` **plus** `FLOWEDGE_API_TOKEN` (refuses to start otherwise)
- Credentials encrypted with AES-256-GCM; workflow exports never leak secrets
- Code-execution node runs a yaegi Go subset with a stdlib allowlist and static import rejection

## License

Apache-2.0
