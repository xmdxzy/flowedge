# FlowEdge v2026.10.20 — Release Notes

First public release. A private, single-binary multimodal workflow engine for one
consumer GPU: chain local LLM / text-to-image / video / ASR / TTS / RAG nodes on
one canvas and run the whole pipeline locally. No Docker, no Node, no Python, no
cloud account.

## Highlights

**Orchestration**
- Visual DAG canvas (React Flow v12), layered parallel execution, diamond joins,
  `join: any`, `continue_on_error`
- 26 node types including LLM, T2I, img2img, inpaint, upscale, Wan/H3 video,
  shot list executor, ASR, TTS, vision, select, RAG, code, HTTP, transform,
  aggregate, loop, sub-workflow, and the new **quality_gate**
- Single-step debugger + breakpoints; resume from any node; branch-aware resume
- Variable system `{{nodeId.path}}`; webhook + cron triggers; versioning with
  rollback; AES-256-GCM credential store; artifact lineage

**New in this cycle**
- `quality_gate` node — frame-exact acceptance (`-count_frames`), A/V drift,
  silence gaps, loudness, energy-based speech detection, wired into the graph so
  a bad segment fails the run before assembly instead of after delivery
- Save-time parameter validation — missing prompts / missing image inputs are
  rejected before they enter the database (`node_param_missing`)
- Run-time dependency preflight — missing Ollama / ASR / vision models fail at
  the entry point with what is missing and how to start it; ComfyUI auto-start
  paths are not blocked (they self-heal)
- Checkpoint compatibility matrix — known VAE-architecture mismatches
  (qwen_image: 12-channel vs 4-channel) fail explicitly at graph-build time
  instead of surfacing as an unexplained `size mismatch` at execution time
- Crash recovery fix — interrupted runs are now reclaimed on **both** the jobs
  and workflow_runs tables (previously runs stayed `running` forever)

**Engineering**
- 123 tests (Go) + TypeScript strict; single-exe Windows build (~32 MB,
  frontend and voice-service scripts embedded)
- Measured VRAM arbitration: resident ComfyUI weights give 6.9× batch throughput;
  image+vision pipeline peaks at 15.0 GB on a 24 GB card with UE5 open
- Explicit failure over silent success: unconfigured services fail loudly and
  never fabricate results

## Known limitations (honest list)

- flux-schnell is a distilled T2I model: img2img needs strength ≥ 0.8 for a real
  style change; inpainting is weak修补 by design (Flux-Fill deferred)
- qwen_image checkpoint is blocked by a ComfyUI VAE mis-detection upstream
  (weights are fine; upgrade ComfyUI to unblock)
- flux prefers English prompts — route Chinese through an LLM node first
- ASR accuracy depends on model tier (default `small`, CPU int8)
- `desktop/` Electron shell is an optional packaging path only

## Install

Download `FlowEdge.exe` from the release assets, double-click, open
http://127.0.0.1:8080. Data lives in `data/` next to the exe; uninstall = delete
the folder. Optional backends (Ollama / ComfyUI / ffmpeg / Python) are detected
and reported by the Environment panel — the engine runs without any of them.

Full documentation: English `README.md`, Chinese `docs/zh/README.md`, film
playbook `docs/en/film-playbook.md` + `docs/多模态出片手册.md`.

Apache-2.0.
