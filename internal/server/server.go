package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/a-h/templ"
	"github.com/isaiah/ghost-radigo/internal/pool"
	"github.com/isaiah/ghost-radigo/internal/server/templates"
)

// Server exposes the HTTP endpoints and streams on-demand audio from the pool.
type Server struct {
	clips      *pool.Pool
	srv        *http.Server
	mu         sync.RWMutex
	nowPlaying templates.NowPlaying
}

// New creates a Server that listens on addr and streams clips from the pool.
func New(addr string, clips *pool.Pool) *Server {
	s := &Server{clips: clips}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		templ.Handler(templates.Page()).ServeHTTP(w, r)
	})
	mux.HandleFunc("/fragment/now", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		np := s.nowPlaying
		s.mu.RUnlock()
		templ.Handler(templates.NowFragment(np)).ServeHTTP(w, r)
	})
	mux.HandleFunc("/now", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		np := s.nowPlaying
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(np) //nolint:errcheck
	})
	mux.HandleFunc("/response", s.handleResponse)

	s.srv = &http.Server{Addr: addr, Handler: mux}
	return s
}

// UpdateNowPlaying records the clip currently being streamed.
// Safe to call from any goroutine.
func (s *Server) UpdateNowPlaying(identifier, title string, year int) {
	s.mu.Lock()
	s.nowPlaying = templates.NowPlaying{
		Identifier: identifier,
		Title:      title,
		Year:       year,
		ArchiveURL: fmt.Sprintf("https://archive.org/details/%s", identifier),
	}
	s.mu.Unlock()
}

// Run starts the HTTP server. It blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.srv.Addr)
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

// handleResponse streams pre-encoded clips from the pool until the client
// disconnects (button released). Each non-silence clip updates nowPlaying
// so the HTMX metadata poll can surface it.
func (s *Server) handleResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	ch := s.clips.C()

	for {
		select {
		case clip, ok := <-ch:
			if !ok {
				return
			}
			if _, err := w.Write(clip.Data); err != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
			if !clip.Silence {
				s.UpdateNowPlaying(clip.Identifier, clip.Title, clip.Year)
			}
		case <-r.Context().Done():
			return
		}
	}
}
