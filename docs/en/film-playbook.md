# Multimodal Film Playbook (FlowEdge + MiniMax-H3) — Selected Chapters

> This is the English distillation of the chapters that carry the transferable
> engineering: route selection and the five-layer control gates. The full
> Chinese playbook (prompt templates, measured parameters, acceptance
> checklists, troubleshooting) lives at
> [`docs/多模态出片手册.md`](../多模态出片手册.md).
> Everything below is measured on one RTX 3090 24 GB, not estimated.

---

## 1. Choose the route first

| What you want | Route | Key point |
|---|---|---|
| A character speaking, lip-sync matters | **ref2va + reference audio** (`h3_r2v`) | Only this route exposes `ref_audios`; `fully_copy` passes audio through 1:1 |
| Ambience / combat only, no dialogue | **fl2va** (`h3` / `h3_i2v`) | Sound fields must be filled with visible facts, or the model invents its own lines |
| Same character across multiple segments | **ref2va + character stills** (`ref_images`) | Identity is locked by image, not by text description |
| Single shot ≤ 15 s | One generation | 124 frames ≈ 5.17 s, 243 ≈ 10.1 s, 362 ≈ 15.1 s |
| Longer than 15 s | **Segment + assemble** (`shot-film.py`) | Every segment carries the same reference set |

**Iron law**: dialogue in T2V is uncontrollable (Chinese degrades into pseudo-Chinese
gibberish in practice). If you need lines, use ref2va. Do not hard-write dialogue into T2V.

---

## 2. Design the gate before you press start

### Why not a fixed duration table

"How many seconds should this shot be" has no constant answer. A 30-second duel and
a 15-second ad have completely different rhythm structures; a different director
cuts the same material differently. So this playbook does not hand you a table of
"correct durations" — it gives you **a mechanism to write down your judgment and
have a machine check it for self-consistency**.

The worst thing a fixed table does is not being wrong — it is **letting people skip
the "think it through first" step**. Hence: starting points + how to adjust them,
with the final answer landing in `plan.gates`.

### The five layers

| Layer | Governs | Key fields |
|---|---|---|
| `timing` | Rhythm | `target_seconds` / `tolerance_seconds` / `min_shot_seconds` / `max_shot_seconds` / `beat_weights` |
| `identity` | Character | `stills` / `require_ref_images_every_segment` / `max_ref_images` / `require_picture_tags` |
| `audio` | Sound | `dialogue.must_transcribe` / `ambience.must_be_silent` / `loudness_db` / `max_silence_gap` / `segment_retry` |
| `technical` | Technical | `min_width` / `min_height` / `must_have_audio` / `av_drift_max` / `fade` |
| `budget` | Cost | `wall_seconds_max` / `per_segment_wall_max` / `on_exceed` |

### The three-step flow (every single time)

```bash
# 1) Draft: give only "total duration + narrative weight per beat", no seconds
python scripts/film-gates.py plan --profile narrative --seconds 30 \
    --beats setup=1.0,clash=1.35,finish=1.2 \
    --modes dialogue,ambience,dialogue --lines-per-seg 1,0,2 \
    --stills 2 --min-shot 1.2 --budget 1500
# → gates draft + suggested frames (weighted, then snapped to the 17k+5 grid)

# 2) Rewrite the draft for THIS film's content into plan.json's gates
#    (adjust min_shot / max_shot / weights / audio policy / budget — do not copy)

# 3) Verify the gate: self-inconsistent means you may not start
python scripts/film-gates.py check --plan data/film/jindan/plan.json
```

`shot-film.py` runs step 3 automatically; a plan without `gates` is refused outright
(`--no-gates` exists for debugging only). A gate failure costs nothing; an un-gated
20-minute render costs 20 minutes — every time it drifts.

### Preset profiles: where to start tuning from

A profile is not the right answer — it is "this film is roughly of this type,
start tuning from these numbers".

| profile | min_shot | max_shot | total tolerance | segment retry | silence gap | over budget |
|---|---|---|---|---|---|---|
| `narrative` (default) | 1.2 | 4.5 | 0.75 | 1 | 1.0 | warn |
| `quick-cut` (4–6 cuts in 15 s) | 0.8 | 2.5 | 0.4 | 1 | 0.6 | warn |
| `long-take` (emotional / monologue) | 2.0 | ∞ | 1.0 | 1 | 1.2 | warn |
| `dialogue` (lip-sync is the mission) | 1.5 | 6.0 | 0.75 | 2 | 0.8 | warn |
| `batch-night` (overnight batch: block, don't drag) | 1.0 | 5.0 | 0.75 | 0 | 1.0 | **fail** |
| `ambience-only` (pure atmosphere) | 1.5 | 6.0 | 0.75 | 1 | 0.7 | warn |

Overlay order: `DEFAULT < PROFILE < this film's gates < explicit CLI flags`.
CLI options therefore default to `None` — a concrete default like 1.2 would
silently override the profile (this exact bug was hit once: `--profile quick-cut`
still had `min_shot = 1.2`).

### How to set weights: the only place judgment is required

Do not ask "which segment should be longer". Ask "how much narrative weight does
this segment carry". Weights are relative; the script distributes proportionally:

| Narrative function | Weight start | Note |
|---|---|---|
| Establishing | 0.8–1.1 | Space + positions, leave as soon as it reads |
| Escalation / clash | 1.2–1.5 | The payload of the piece |
| Turn / reveal | 1.1–1.4 | Needs air, but not a long take |
| Resolution / fallout | 0.9–1.2 | Land it, don't linger |

---

## 3. The acceptance four-piece (why duration checks are not enough)

`cut` with a 0.04 s fade once dropped an **entire segment** — duration and audio
were both perfect. Only frame counting catches this. Before delivery:

```bash
python scripts/verify-media.py final.mp4 --asr --frames .tmp/frames
```

Checks: stream list + duration + **exact frame count** + A/V drift + mid-file
silence gaps + loudness + ASR transcription. Any `FAIL` blocks delivery.

The rule behind the tool: **a check that cannot fail is not a gate** (WARN is a
memo). And the tool must distinguish *unreachable* from *empty result* — an
unreachable ASR service returning empty text looks exactly like "no speech
detected", which is how acceptance gets silently skipped.
