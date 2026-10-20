# Changelog

All notable changes to FlowEdge are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project adheres to
date-based versioning (`vYYYY.MM.DD`).

## [v2026.10.20]

### Added
- **Deep health checks for the local speech services** — `/health?deep=1` no longer
  just reports "the process is alive". TTS synthesises a throwaway sample end to end;
  ASR verifies that every module it needs can actually be imported, including
  transitively-required ones. Previously a service could answer `/health` with 200 and
  still return 500 on the first real call (`No module named 'ordered_set'`), so a run
  wasted 100+ seconds before surfacing an error that was knowable up front.
  Old service builds that do not understand the query string fall back cleanly to plain
  `/health`, so upgrading the engine alone does not break a running service.
- **`Degraded` is now a first-class dependency state** — separate from "not running".
  A degraded dependency reports the exact `pip install` command and is deliberately
  *not* marked one-click-fixable: restarting a process that is already alive never
  fixes a missing package.
- **`ordered_set` pinned in `requirements.txt`** — it is a transitive dependency of
  faster-whisper that is not always installed automatically; the service starts fine
  without it and only fails at transcription time.
- **P0 Observability** — long multimodal nodes used to render as a spinner with no
  signal. The engine now emits a `node_progress` heartbeat every 2s (`elapsed_ms` /
  `eta_ms`) while a node runs, and stamps `duration_ms` on `node_finished`,
  `node_failed` and cache hits. ETAs come from a per-node-type EWMA of observed
  durations and return `0` when there is no history — never a fabricated number.
- **P1 Orchestration depth** — `SetMaxParallel(n)` caps how many nodes of one
  topological layer may run at once (0 = unlimited). Independent branches still run
  concurrently; heavy nodes stay serialized by the VRAM arbiter.
- **P2 Scene lock for image nodes** — `text2image` / `image2image` now resolve and inject
  the SCENE LOCK compiled by an upstream `scene_grid` node (or a hand-filled
  `scene_lock` / `scene_grid` field). Video was already covered; storyboards and
  character sheets were not, which is why the same character drifted across shots.
  Injection is pass-through: a prompt that already carries a lock is left byte-identical,
  so the `h3.scene_lock_missing` / `h3.scene_lock_drift` gates stay meaningful.
- **P2 Actionable gate fixes** — every failing `quality_gate` check now carries a `fix`
  string naming the exact knob to turn (raise `width` from 640 to ≥1280; raise `frames`
  to ≥124; add `-shortest`; drop the empty TTS segment…), and the blocking error text
  includes them. A gate that only reports a code hands the debugging cost right back.
- **P3 VRAM degrade-and-retry** — OOM is a capacity signal, not bad luck. Generation now
  releases diffusion VRAM (waiting for the asynchronous CUDA free), then steps down
  one notch at a time — resolution ×0.75 (8-aligned) → frames ×0.7 → steps ×0.6 — up to
  twice. Retrying in-process is an order of magnitude cheaper than failing and
  reloading 34GB of weights. Degradation is recorded in the node output
  (`degraded` / `degrade_level` / `degrade_note`).
- **P3 Dependency health check with one-click fix** — `POST /api/preflight` checks the
  current canvas without running it and returns each missing dependency with a concrete
  fix action and whether the engine can start it for you (`fixable` + `fix_dep`).
  The UI shows the report with a 一键修复 button per fixable item.
- **Text-to-image Chinese auto-translation** — when a node enables `auto_translate`,
  the engine translates CJK prompts to English with an LLM *before* they enter the
  diffusion stage (keeps GPU-side model loads from thrashing). Output carries
  `translated` / `translate_error` fields. Default off.
- **ASR tiering via node fields** — the `asr` node now forwards `model` / `device`
  to faster-whisper. The Python ASR service keeps a per-(model,device) cache and
  enforces a device allowlist (rejects `rocm`, etc.).
- **Inpaint model override** — the `inpaint` node accepts a `model` field to swap
  the default checkpoint (e.g. a dedicated Flux-Fill inpainter) instead of the
  weak default修补 path; empty = default.
- **Linux / macOS one-click run** — `scripts/run-local.sh` builds on demand
  (only when source is newer than the binary), launches in the background, health
  polls `/api/health` (bypassing the local proxy to avoid false 502s), and supports
  `--stop` / `--logs` / `--foreground` / `--port`.
- **`quality_gate` node** — five-layer gate (timing / identity / audio / technical /
  budget) that fails the run when any hard check is violated, so bad outputs never
  ship silently.
- **Parameter validation & dependency preflight** — node inputs are validated before
  execution; missing external services (ComfyUI / Ollama / ASR / TTS) are detected up
  front with actionable messages instead of mid-run crashes.
- **VAE compatibility matrix** — `comfy_compat.go` maps checkpoints to the correct VAE
  so cross-framework loads don't silently produce broken tensors.
- **English README, CONTRIBUTING, CI workflow, LICENSE (MIT)**.
- **`build-windows.py` cross-platform** — `--goos` / `--goarch` / `--output` reuse the
  same frontend + local-service embedding for Linux/macOS cross builds.

### Fixed
- **VRAM arbiter could be created twice under parallel layers** — `arbiter()` did an
  unlocked lazy init, so the *first* batch of heavy nodes after engine start
  (which the scheduler launches concurrently) could each allocate their own
  arbiter and each acquire their own `heavyMu`. Two diffusion nodes would then
  enter ComfyUI simultaneously and OOM a 24GB card. The lazy init is now
  mutex-guarded. Caught by `TestHeavyNodesSerializeOnVRAMArbiter`, whose
  concurrency probe made the bug surface as an intermittent failure —
  the scheduler sometimes got lucky and interleaved without overlap.

- **Stale `running` runs never recovered** — `queue.go` `RecoverInterrupted` now also
  sweeps the `workflow_runs` table, not just the task table, so interrupted runs are
  correctly marked failed instead of hanging forever.
- **Empty smoke-test database** — the 0-row `data/flowedge.db` shell was renamed;
  51 redundant smoke workflows purged from the real DB.
- **Dependency mismatch crashes** — preflight now fails fast with a clear message.

### Verified
- `go test ./...` green (189 in `service`, 192 total across packages); frontend `tsc --noEmit` clean.
- `TestHeavyNodesSerializeOnVRAMArbiter` / `TestParallelBranchesOverlap` pass under `-count=5`
  (concurrency probes are timing-based; repeated runs were used to rule out flakiness).
- Linux `GOOS=linux GOARCH=amd64` cross build produces a 31 MB CGO-free ELF.
