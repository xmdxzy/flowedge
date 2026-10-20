# Changelog

All notable changes to FlowEdge are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/), and this project adheres to
date-based versioning (`vYYYY.MM.DD`).

## [v2026.10.20]

### Added
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
- **Stale `running` runs never recovered** — `queue.go` `RecoverInterrupted` now also
  sweeps the `workflow_runs` table, not just the task table, so interrupted runs are
  correctly marked failed instead of hanging forever.
- **Empty smoke-test database** — the 0-row `data/flowedge.db` shell was renamed;
  51 redundant smoke workflows purged from the real DB.
- **Dependency mismatch crashes** — preflight now fails fast with a clear message.

### Verified
- `go test ./...` green (125 tests); frontend `tsc --noEmit` clean.
- Linux `GOOS=linux GOARCH=amd64` cross build produces a 31 MB CGO-free ELF.
