# Contributing to FlowEdge

Thanks for your interest. FlowEdge is a local-first multimodal workflow engine:
one binary, one GPU, no cloud. The guidelines below exist to keep it that way.

## Ground rules

1. **Explicit failure over silent success.** If a service is missing or a
   parameter is invalid, fail loudly with an actionable message. Never fabricate
   a result (no fake audio, no fake transcriptions, no placeholder passes).
2. **Root-cause fixes, no workarounds.** A bug is closed as
   symptom → root cause → fix. Patching a symptom with a tuned parameter is not
   a fix.
3. **One variable per change.** Don't mix refactors, parameter changes, and new
   features in one commit. If a batch touches two or more independent things,
   say so in the commit body and keep A/B evidence.
4. **Gates must have teeth.** A check that cannot FAIL is not a gate. New
   validation must be able to block a run.
5. **Measure, don't estimate.** Time and memory claims in docs/commits must come
   from a real run on stated hardware, with the parameters stated.

## Development setup

```bash
# backend
cd backend && go mod tidy && go run main.go

# frontend
cd frontend && npm install && npm run dev

# everything, before you push
cd backend && go build ./... && go vet ./... && go test ./... -count=1
cd frontend && npx tsc --noEmit
```

## Adding a node type

1. Backend, two files, both required: the scheduling metadata in
   `backend/service/nodespec.go` (does it need the GPU, which VRAM side, timeout)
   and the canvas contract in `backend/service/nodecatalog.go` (ports, fields,
   cost, label). `nodecatalog_test.go` fails if the two key sets drift apart.
2. Implement the executor in `backend/service/`, register it in the
   `nodeRegistry` map in `engine_v2.go`, and add its timeout case in
   `nodeTimeoutFor`.
3. Nothing in the frontend hardcodes node types. The palette, the inspector and
   the edge validation all read the catalog, so a correct backend entry is the
   whole UI job. If you find yourself editing a list of node types in
   `frontend/src`, something has gone wrong.
4. Write one test that proves the node fails correctly on bad input. A node with
   only a happy-path test is not done.

## Pull requests

- Keep PRs focused; one concern per PR.
- Include the test evidence (paste the relevant `go test` output).
- Update `README.md` / `docs/` in the same PR if behavior changed.
- No new dependencies without discussion — local-first and zero-install are
  hard constraints.

## Reporting bugs

Include: what you ran (workflow JSON or node config), what happened, what you
expected, and the engine's console output. Screenshots of the execution panel
(event stream) are more useful than screenshots of the canvas.
