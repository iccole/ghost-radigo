package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
)

//go:embed static/index.html
var indexHTML []byte

// NowPlaying holds metadata for the clip currently being streamed.
type NowPlaying struct {
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	Year       int    `json:"year"`
	ArchiveURL string `json:"archive_url"`
}

// Server exposes the /listen HTTP endpoint, broadcasting the audio stream
// to all connected clients.
type Server struct {
	stream     *Broadcast
	srv        *http.Server
	mu         sync.RWMutex
	nowPlaying NowPlaying
}

// New creates a Server that listens on addr.
func New(addr string) *Server {
	b := newBroadcast()
	s := &Server{stream: b}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML) //nolint:errcheck
	})
	mux.HandleFunc("/listen", s.handleListen)
	mux.HandleFunc("/now", s.handleNow)

	s.srv = &http.Server{Addr: addr, Handler: mux}
	return s
}

// UpdateNowPlaying records the clip currently being mixed into the stream.
// Safe to call from any goroutine.
func (s *Server) UpdateNowPlaying(identifier, title string, year int) {
	s.mu.Lock()
	s.nowPlaying = NowPlaying{
		Identifier: identifier,
		Title:      title,
		Year:       year,
		ArchiveURL: fmt.Sprintf("https://archive.org/details/%s", identifier),
	}
	s.mu.Unlock()
}

// Write sends audio bytes to all currently connected listeners.
// It satisfies io.Writer so it can be passed directly to the Mixer.
func (s *Server) Write(p []byte) (int, error) {
	return s.stream.Write(p)
}

// Run starts the HTTP server. It blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr())
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		s.srv.Shutdown(context.Background()) //nolint:errcheck
	}()
	if err := s.srv.Serve(ln); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) addr() string { return s.srv.Addr }

// handleNow returns the currently playing clip as JSON.
func (s *Server) handleNow(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	np := s.nowPlaying
	s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(np) //nolint:errcheck
}

// handleListen registers the client, streams audio until the connection drops.
func (s *Server) handleListen(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	id, ch := s.stream.Register()
	defer s.stream.Deregister(id)

	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}
