# Ghost Radio

A Go service that turns the Internet Archive into a spirit box. You ask a question and the station sweeps across decades of public-domain recordings: old-time radio, LibriVox readings, spoken-word archives, and the audio tracks of old Flash cartoons. It pulls sub-second fragments of sound, stitches them into short "transmissions" and plays them back as your answer.

![Ghost Radio](docs/screenshot.png)

## How it works

The backend is a pipeline of long-running goroutines connected by buffered channels. When a downstream stage falls behind, the channel fills up and the stage before it waits.

```
Fetcher ──▶ Scanner ──▶ Processor pool (8 workers) ──▶ Session manager ──▶ HTTP / SSE
  │            │               │                              │
  IA search    random walk     ffmpeg subprocesses            stitches fragments
  + metadata   through time    + disk cache                   into 7–10 s composites
```

| Stage | Package | What it does |
|---|---|---|
| **Fetcher** | `internal/fetcher` | Rotates through Internet Archive search queries across speech-heavy collections. It resolves each item's metadata to pick the best streamable file (MP3/Ogg, with video containers as a fallback), skips access-restricted items and backs off when rate-limited. |
| **Scanner** | `internal/scanner` | The "station dial." It keeps a `currentYear` that does a random walk between 1900 and 1980: mostly small drifts, with an occasional large jump. It ignores anything dated outside that range, picks media weighted by how close each item's year is to the dial and emits `ClipJob`s for 0.1–2 s fragments, with sometimes a burst of static instead. |
| **Processor** | `internal/processor` | A worker pool that shells out to `ffmpeg`. Each job first runs a cheap voice-activity probe (bandpass + `volumedetect`) to reject dead air. Then ffmpeg seeks into the remote file, applies fades, loudness normalization and a bandpass filter, and reverses about 15% of clips for an EVP effect before encoding to MP3. Pink-noise gaps come from ffmpeg's `anoisesrc`. |
| **Cache** | `internal/cache` | A disk cache for encoded clips that persists across restarts. Keys quantize the offset into 5 s buckets, so nearby picks reuse work. Writes are atomic (temp file + rename), and a `nil` cache is a valid no-op. |
| **Session manager** | `internal/session` | The only consumer of processor output. It fans each fragment out to every active listener session, and each session collects fragments into 7–10 s composites until it has received five. |
| **Server** | `internal/server` | Standard-library `net/http` with Go 1.22 method/path routing. It serves a [templ](https://templ.guide) page, a JSON endpoint to open a session, a Server-Sent Events stream announcing each composite as it lands, and an endpoint that serves the MP3 bytes. |

Go never decodes or encodes audio itself. All signal processing happens in `ffmpeg` subprocesses, bounded by `context` timeouts so a slow remote seek can't stall a worker.

### Design notes

- **Concurrency through channels and context.** Every stage exposes `Run(ctx) error` and stops cleanly on `SIGINT`/`SIGTERM` through `signal.NotifyContext`.
- **Single-writer state.** A session's accumulation buffer is only touched by the manager goroutine, so it needs no lock. Only the finished composites, which HTTP handlers read, sit behind an `RWMutex`.
- **Fail open.** If the VAD probe errors (network hiccup, odd format), the clip is accepted rather than dropped, because keeping the sound flowing matters more than filtering perfectly.
- **Deterministic randomness.** The scanner runs from a seeded `math/rand` source, so a run can be reproduced.

## API

| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Web UI |
| `POST` | `/session` | Open a listening session → `{"id": "..."}` |
| `GET` | `/session/{id}/events` | SSE stream: one `clip` event per composite (`index`, `identifier`, `title`, `year`), then `done` |
| `GET` | `/clip/{id}/{n}` | MP3 bytes for composite `n` |
| `DELETE` | `/session/{id}` | Stop and discard a session |

## Running it

Requirements: Go 1.24+ and `ffmpeg` on your `$PATH`.

```bash
go run ./cmd/station
# open http://localhost:8080
```

Or with Docker (the image includes ffmpeg):

```bash
docker compose up --build
```

Encoded clips are cached in your OS cache directory (`~/Library/Caches/ghost-radio` on macOS). Set `GHOST_RADIO_CACHE_DIR` to override it.

The first session after a cold start can take a little while, because the fetcher has to fill its buffer from archive.org before the scanner has anything to tune into.

## Project layout

```
cmd/station/          entry point: wires the pipeline together
internal/fetcher/     Internet Archive search + metadata client
internal/scanner/     random-walk "dial" and clip selection
internal/processor/   ffmpeg worker pool, VAD probe, noise generation
internal/cache/       on-disk clip cache
internal/session/     per-listener clip accumulation
internal/server/      HTTP handlers and templ UI
internal/logger/      leveled logging
```

## Content

All audio is streamed directly from the [Internet Archive](https://archive.org) and comes from public-domain or openly licensed collections. Nothing is redistributed or stored beyond the local clip cache.
