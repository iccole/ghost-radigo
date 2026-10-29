# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**Ghost Radio** is a Go service that acts like a "spirit box" over the Internet Archive. A listener asks a question in the web UI; the station sweeps across public-domain recordings (old-time radio, LibriVox, spoken-word collections, Flash cartoon audio), extracts sub-second fragments, stitches them into short composites, and plays five of them back as the "answer". There are no playlists, no search, and no user control over what is selected.

## Commands

```bash
# Build
go build ./...

# Run (serves on :8080)
go run ./cmd/station

# Test
go test ./...

# Vet / lint (golangci-lint assumed installed)
go vet ./...
golangci-lint run

# Format
gofmt -w .

# Regenerate templ output after editing templates.templ
templ generate

# Docker (image bundles ffmpeg)
docker compose up --build
```

**External dependencies:** `ffmpeg` must be installed and on `$PATH`. Transcription is optional: it needs whisper.cpp's `whisper-cli` on `$PATH` and `GHOST_RADIO_WHISPER_MODEL` pointing at a ggml model (`base.en` recommended; the Docker image bundles both). Without them the station logs "transcription off" and joins pieces by pause roles only.

`internal/server/templates/templates_templ.go` is generated but committed, so the project builds without the `templ` CLI. Always regenerate it after editing `templates.templ` — never hand-edit the generated file.

## Architecture

A linear chain of long-running goroutines connected by buffered channels (backpressure is implicit):

```
Fetcher → Scanner → Processor pool (+ Transcriber) → Session manager → HTTP (SSE + clip bytes)
```

Every stage exposes `Run(ctx context.Context) error` and is started from `cmd/station/main.go`, which owns the channels and shuts everything down via `signal.NotifyContext`.

### Stages

**Fetcher** (`internal/fetcher/`)
- Rotates through `searchQueries` against the IA advanced search API, favouring speech-heavy collections (`oldtimeradio`, `librivox`, `opensource_audio`, …) plus Flash animation collections as `mediatype:movies`.
- `collection:radio` and music-heavy collections are intentionally excluded (long modern files cause ffmpeg seek timeouts; speech fits the concept better).
- Filters out `access-restricted` items; resolves `/metadata/{id}` to pick the best file (MP3/Ogg preferred, video containers as fallback).
- Emits `MediaItem` (file URL, title, year, duration). Backs off on errors and rate limits.

**Scanner** (`internal/scanner/`) — the "dial"
- Holds `currentYear` (1900–1980) doing a random walk: small drifts (≤3 yrs) with occasional large jumps (≤15 yrs, 10%).
- Drops items dated outside 1900–1980 on intake; picks the rest weighted by proximity to `currentYear` (unknown year = low but nonzero weight).
- Emits `ClipJob`s for 12 s analysis windows, offsets snapped to a 5 s grid (matches the cache buckets) and capped at 15 min into a file; ~15% are `Silence` jobs (0.3–1.5 s noise gaps).
- Seeded `math/rand` — seed comes from `main.go` (0 = time-derived).

**Processor** (`internal/processor/`)
- `Pool` of N workers (`processorCount = 8` in main), each shelling out to `ffmpeg` with a context timeout.
- Flow per window job: load window (cache, else one ffmpeg fetch with `loudnorm` + highpass 80 Hz / lowpass 8 kHz into a local temp MP3) → analyse → cut pieces → encode each piece from the local file.
- Analysis (`analysis.go`): two ffmpeg passes over the local window — 20 ms voice-band RMS (`astats`), and spectral centroid (`aspectralstats`, which re-frames into its own 128 ms windows every 64 ms, so it **must not share a pass** with the RMS chain or the 20 ms grid breaks). Go only reads the numbers:
  - **Speech check** — coefficient of variation of the spectral centroid over voiced frames must be ≥ `minCentroidCV` (0.22); music holds a steady timbre, speech doesn't. Windows that fail are dropped (`errNotSpeech`).
  - **Pause detection** — threshold is relative to the window's own 10th/95th percentile levels (fixed dB thresholds fail on hissy archive audio); runs ≥ 100 ms are pauses. Pieces are the 0.2–2.0 s spans bounded by pauses on both sides, padded 40 ms into each pause.
  - **Roles** — a pause ≥ 400 ms before a piece makes it an `opening`, after it an `ending` (falling sentence pitch), both a `whole` phrase, neither a `middle`.
  - Thresholds were calibrated by hand on OTR, LibriVox, comedy LPs and 78rpm dance/jazz; the numbers are in the constant comments.
- Transcription (`words.go`): if a `Transcriber` is set, ffmpeg converts the window to 16 kHz WAV and `whisper-cli` returns timed words. `annotate` gives each piece the words whose midpoint it contains — if they're confident (mean ≥ 0.5) and not Whisper non-speech markers (`♪`, `(…)`, `[…]`) — plus `grammar` tags of its first/last word, and re-derives its role: opening = after `.?!` or capitalised; ending = ends in `.?!`, or pause-based ending whose last word `grammar.CanEnd`. Failures are logged and pieces keep pause roles.
- Up to `maxPiecesPerWindow` (4) random pieces per window, transcribed ones first. Pieces get 15/30 ms anti-click fades, MP3 128k/44.1k stereo; ~5% are `areverse`d for an EVP effect (which swaps opening ↔ ending).
- `EncodeSilence` produces the digital-silence gaps that `main.go` pre-encodes at startup for the session.
- Silence jobs become quiet pink noise via ffmpeg `anoisesrc`.

**Transcriber** (`internal/transcribe/`)
- Wraps `whisper-cli` (`-ml 1 -sow -ojf`: one JSON segment per word). CPU only (`-ng`), 2 threads, at most 4 concurrent calls (semaphore). Word confidence = lowest probability of its lexical tokens (punctuation tokens are ignored — they score low on clear words).
- Nil-safe like the cache: a nil `*Transcriber` returns no words.

**Grammar** (`internal/grammar/`)
- `Of(word)` returns a bit-set `Tag` of possible parts of speech (closed-class lexicon + common irregular verbs/adjectives + suffix rules). `CanFollow(prev, next)` checks a junction against the `follows` table; zero (unknown) tags fit anywhere. Intentionally small: only the words either side of a junction matter.
- Outputs `ProcessedClip` (MP3 bytes + metadata).

**Cache** (`internal/cache/`)
- On-disk cache of normalised 12 s **windows** (not pieces), keyed by `identifier_{offset bucketed to 5s}_{duration rounded}s.mp3`. A hit skips the network fetch; analysis and piece cutting still run.
- Location: `$GHOST_RADIO_CACHE_DIR`, else `os.UserCacheDir()/ghost-radio`.
- Atomic writes (tmp + rename). Methods are nil-safe — a nil `*Cache` means "no cache".

**Session manager** (`internal/session/`)
- Sole consumer of processor output. Drops silence clips and fans each piece out to every active session.
- Each `Session` pools pieces by role (≤ `poolCap` each, oldest dropped) and assembles phrases: opening → 1–3 middles → ending, or a single whole piece. Word gaps (60–150 ms) go inside a phrase, phrase gaps (300–600 ms) between phrases; both are pre-encoded MP3 silences passed in as `Gaps`.
- Candidates must satisfy `grammar.CanFollow(lastTag, first)`; among those, `score` prefers transcribed (+4), a different recording than the last piece (+2), and 3+ words (+1). `lastTag` resets to 0 at each phrase end.
- When any pool is full, `takeAny` relaxes the role first (best grammatical piece of any role), then grammar (fullest pool) — continuity over accuracy.
- Each published composite is logged with its transcript (`"text"`, `…` untranscribed, `/` phrase break) — the quickest way to judge assembly quality.
- A composite is published when a phrase ends at or past its 7–10 s target; once past the target, a phrase skips its remaining middles. After `MinClips` (5) composites the SSE handler stops the session.
- Concurrency rule: assembly state (`pools`, `buf`, `bufDur`, `targetDur`, `next`, `middlesLeft`, `lastID`, `lastTag`, `rng`) is touched only by the `Manager.Run` goroutine — no lock. `composites` is read by HTTP handlers and guarded by `mu`.

**Server** (`internal/server/`)
- Standard `net/http` with Go 1.22 method/pattern routing; UI rendered with templ (`templates/`).

| Route | Purpose |
|---|---|
| `GET /` | Web UI |
| `POST /session` | Create session → `{"id"}` |
| `GET /session/{id}/events` | SSE: `clip` events (`index`, `identifier`, `title`, `year`), then `done` |
| `GET /clip/{id}/{n}` | MP3 bytes for composite `n` |
| `DELETE /session/{id}` | Stop and remove session |

The front end (inline JS in `templates.templ`) handles the question form, plays composites sequentially via a single `Audio` element, and drives the visual effects (grain, year counter, sweep).

**Logger** (`internal/logger/`) — small leveled logger (`Debug/Info/Warn/Error`); use it instead of `log` directly.

### Legacy

`internal/pool/` (slices clips into ~150 ms chunks for a continuous ghost-box stream) is from the earlier continuous-stream design and is **not currently wired in**. Don't build on it without deciding whether to revive or delete it.

### Design rules

- **ffmpeg does all audio work** — never decode/encode audio in-process in Go. `whisper-cli` only analyses (it reads the WAV ffmpeg produced). Concatenating already-encoded MP3 byte slices (session composites, including the silence gaps) is the only byte-level manipulation.
- **Continuity over accuracy** — prefer keeping sound flowing over perfect filtering. Exception: windows that fail the speech check or analysis are skipped, since pieces can't be cut without an envelope.
- **Stateful station, anonymous listeners** — sessions are ephemeral, in-memory, identified only by a random ID.
- **No external services** beyond archive.org — no database, no Redis. Transcription runs locally.
- Every goroutine must exit on `ctx.Done()`; every ffmpeg call must have a timeout context.

## Internet Archive API

- Search: `GET https://archive.org/advancedsearch.php?q=...&output=json`
- Item metadata: `GET https://archive.org/metadata/{identifier}`
- File download: `https://archive.org/download/{identifier}/{filename}`

Set a descriptive `User-Agent` (see `fetcher/archive.go`) and respect rate limiting.
