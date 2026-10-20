# Good first issues

A curated starter list for new contributors. Each item is scoped to be completable
without deep knowledge of the GPU stack, and each has a clear definition of done.
Maintainers: copy these into GitHub Issues with the `good first issue` label.

## 1. Add GitHub Actions badge and wire the release workflow

The CI workflow exists (`.github/workflows/ci.yml`) but there is no release
workflow. Add one that tags a release and uploads `dist/FlowEdge.exe` as an asset.

**Done when:** pushing a `v*` tag produces a draft release with the exe attached;
the README shows a CI badge.

## 2. Node help texts

Every node type's configuration panel should carry a one-sentence "what this
does / what it needs" hint (several already do — see `shotlist` and `quality_gate`
in `frontend/src/App.tsx`). Audit the remaining types and add hints where missing.

**Done when:** every entry in `PALETTE` renders a hint in its config panel.

## 3. Error message audit

Walk `backend/service` for `fmt.Errorf` calls whose message does not tell the
user what to do next (e.g. "invalid input" with no field name). Rewrite them to
the pattern: what is wrong + which field + how to fix.

**Done when:** `grep -rn 'fmt.Errorf' backend/service | grep -viE '节点|缺少|未|请|失败|超时'` returns only cases where the message is already actionable.

## 4. `docs/en/` coverage for the remaining playbook chapters

`docs/en/film-playbook.md` covers route selection and the five-layer gates.
Translate the remaining chapters of `docs/多模态出片手册.md` (prompt templates,
measured parameters, acceptance checklist, troubleshooting) — the source is
already battle-tested; this is translation, not invention.

**Done when:** every chapter of the Chinese playbook has an English counterpart.

## 5. Windows service script parity

`运行/1-启动 FlowEdge.bat` starts the engine detached on Windows. There is no
equivalent documented one-liner for Linux/macOS (beyond `go run`). Write a
`scripts/run-local.sh` that does the same job (build-if-needed + start + health
poll) and document it in the README.

**Done when:** a clean clone on Linux can reach `http://127.0.0.1:8080` after
running one script; the README links it.

## 6. `quality_gate` presets

The `quality_gate` node takes raw rule fields. Add a `preset` field (like the
`film-gates.py` profiles: narrative / dialogue / ambience-only / batch-night)
that expands into the individual rules, with CLI/data overrides winning.

**Done when:** `TestNodeRegistry_HasAllBuiltins` still passes, and a new test
proves `preset=dialogue` expands to the documented rule set.

## Rules for all of the above

- One issue = one PR. Include test evidence.
- Follow `CONTRIBUTING.md`: explicit failure, no new dependencies, gates with teeth.
