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

**External dependency:** `ffmpeg` must be installed and on `$PATH`.

`internal/server/templates/templates_templ.go` is generated but committed, so the project builds without the `templ` CLI. Always regenerate it after editing `templates.templ` — never hand-edit the generated file.

## Architecture

A linear chain of long-running goroutines connected by buffered channels (backpressure is implicit):

```
Fetcher → Scanner → Processor pool → Session manager → HTTP (SSE + clip bytes)
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
- Emits `ClipJob`s of 0.1–2.0 s fragments; ~15% are `Silence` jobs (0.3–1.5 s noise gaps). Offsets are capped at 15 min into a file.
- Seeded `math/rand` — seed comes from `main.go` (0 = time-derived).

**Processor** (`internal/processor/`)
- `Pool` of N workers (`processorCount = 8` in main), each shelling out to `ffmpeg` with a context timeout.
- Flow per job: cache lookup → VAD probe (`vad.go`: bandpass + `volumedetect`, rejects near-silence; **fails open** on probe errors) → encode.
- Encode chain: seek, trim, random 0.3–0.8 s fades, `loudnorm`, highpass 80 Hz / lowpass 8 kHz, MP3 128k/44.1k stereo. ~15% of clips are `areverse`d for an EVP effect.
- Silence jobs become quiet pink noise via ffmpeg `anoisesrc`.
- Outputs `ProcessedClip` (MP3 bytes + metadata).

**Cache** (`internal/cache/`)
- On-disk MP3 cache keyed by `identifier_{offset bucketed to 5s}_{duration rounded}s.mp3`.
- Location: `$GHOST_RADIO_CACHE_DIR`, else `os.UserCacheDir()/ghost-radio`.
- Atomic writes (tmp + rename). Methods are nil-safe — a nil `*Cache` means "no cache".

**Session manager** (`internal/session/`)
- Sole consumer of processor output. Drops silence clips and fans each fragment out to every active session.
- Each `Session` accumulates fragments into a composite of 7–10 s, then publishes it and signals `Notify`. After `MinClips` (5) composites the SSE handler stops the session.
- Concurrency rule: accumulation state (`buf`, `bufDur`, `targetDur`, `rng`) is touched only by the `Manager.Run` goroutine — no lock. `composites` is read by HTTP handlers and guarded by `mu`.

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

- **ffmpeg does all audio work** — never decode/encode audio in-process in Go. Concatenating already-encoded MP3 byte slices (session composites) is the only byte-level manipulation.
- **Continuity over accuracy** — prefer keeping sound flowing over perfect filtering (e.g. VAD fails open).
- **Stateful station, anonymous listeners** — sessions are ephemeral, in-memory, identified only by a random ID.
- **No external services** beyond archive.org — no database, no Redis.
- Every goroutine must exit on `ctx.Done()`; every ffmpeg call must have a timeout context.

## Internet Archive API

- Search: `GET https://archive.org/advancedsearch.php?q=...&output=json`
- Item metadata: `GET https://archive.org/metadata/{identifier}`
- File download: `https://archive.org/download/{identifier}/{filename}`

Set a descriptive `User-Agent` (see `fetcher/archive.go`) and respect rate limiting.
