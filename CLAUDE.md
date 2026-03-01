# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**Ghost Radio** is a Go backend service that generates a continuous, audio-only HTTP stream assembled in real time from public-domain content sourced from the Internet Archive. It behaves like a "radio scanner" — drifting across archival recordings without user control, playlists, or personalization.

## Commands

```bash
# Build
go build ./...

# Run
go run ./cmd/station

# Test
go test ./...

# Test single package
go test ./internal/scanner/...

# Test with verbose output
go test -v ./...

# Lint (assumes golangci-lint installed)
golangci-lint run

# Format
gofmt -w .
```

**External dependency:** `ffmpeg` must be installed and available on `$PATH`.

## Architecture

The pipeline is a linear chain of goroutines connected by channels:

```
Fetcher → Scanner → Processor workers → Mixer → HTTP Stream
```

Each stage is a long-running goroutine. Channels provide natural backpressure between stages.

### Stage responsibilities

**Fetcher** (`internal/fetcher/`)
- Periodically queries the Internet Archive metadata API.
- Retrieves candidate items with audio or video files (video treated as audio containers).
- Caches lightweight metadata only (no full file downloads until clip extraction time).
- Pushes `MediaItem` structs (streamable URL + metadata) into a channel consumed by the Scanner.

**Scanner** (`internal/scanner/`)
- The "station dial" — the core stateful component.
- Maintains `current_year float64`, a session seed, and drift parameters.
- Performs a random walk across time and collections rather than pure random selection.
- Selects short segments (3–20 seconds) from chosen media.
- Emits `ClipJob` structs (URL, time offset, duration) into the processor input channel.
- Behavior: prefer gradual temporal drift, avoid abrupt jumps, allow occasional silence.

**Processor** (`internal/processor/`)
- Spawns `ffmpeg` subprocesses — Go does not decode audio itself.
- Each worker receives a `ClipJob`, runs ffmpeg to: seek to offset, trim duration, normalize loudness, apply soft bandpass filter, add fade-in/out (300–800ms), encode to target format (MP3 or raw PCM).
- Multiple parallel workers allowed.
- Outputs processed audio byte streams into the Mixer input channel.

**Mixer** (`internal/mixer/`)
- Single goroutine maintaining a continuous playback buffer.
- Overlaps clip fades to prevent hard cuts.
- Optionally inserts silence gaps.
- Maintains constant bitrate output.
- Reads from Processor channel, writes to the HTTP stream.

**Stream Server** (`internal/server/`)
- `GET /listen` — live audio stream, infinite response body.
- No seeking, no rewinding.
- Multiple simultaneous listeners receive the same ongoing stream (broadcast).
- Standard `net/http`.

### Key design rules

- **Continuity over accuracy** — smooth transitions matter more than content correctness.
- **Stateless users, stateful station** — the station process holds all state; clients are anonymous.
- **No user-facing controls** — the `/listen` endpoint is the only endpoint that matters.
- **ffmpeg does all audio work** — never attempt in-process audio decoding/encoding in Go.
- **Optional Redis** — only if in-memory buffering proves insufficient; prefer avoiding it.

## Entry Point

`cmd/station/main.go` — wires all stages together and starts the HTTP server.

## Internet Archive API

Base URL: `https://archive.org`
- Search: `GET /advancedsearch.php?q=...&output=json`
- Item metadata: `GET /metadata/{identifier}`
- File download: `https://archive.org/download/{identifier}/{filename}`

Media items must be filtered to audio/video mediatype with actual downloadable files.
