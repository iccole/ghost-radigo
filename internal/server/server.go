package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/a-h/templ"
	"github.com/isaiah/ghost-radigo/internal/session"
	"github.com/isaiah/ghost-radigo/internal/server/templates"
)

// Server exposes the HTTP endpoints.
type Server struct {
	sessions *session.Manager
	srv      *http.Server
}

// New creates a Server that listens on addr and streams clips via sessions.
func New(addr string, sessions *session.Manager) *Server {
	s := &Server{sessions: sessions}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		templ.Handler(templates.Page()).ServeHTTP(w, r)
	})
	mux.HandleFunc("POST /session", s.handleCreateSession)
	mux.HandleFunc("GET /session/{id}/events", s.handleSessionEvents)
	mux.HandleFunc("GET /clip/{id}/{n}", s.handleClip)
	mux.HandleFunc("DELETE /session/{id}", s.handleStopSession)

	s.srv = &http.Server{Addr: addr, Handler: mux}
	return s
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

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.Create()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": sess.ID}) //nolint:errcheck
}

func (s *Server) handleStopSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if sess, ok := s.sessions.Get(id); ok {
		sess.Stop()
		s.sessions.Remove(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.sessions.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	sent := 0
	for {
		select {
		case <-sess.Notify:
			count := sess.Count()
			for sent < count {
				clip, ok := sess.Clip(sent)
				if !ok {
					break
				}
				data, _ := json.Marshal(map[string]any{
					"index":      clip.Index,
					"identifier": clip.Identifier,
					"title":      clip.Title,
					"year":       clip.Year,
				})
				fmt.Fprintf(w, "event: clip\ndata: %s\n\n", data)
				flush()
				sent++
			}
			if sent >= session.MinClips {
				sess.Stop()
				fmt.Fprint(w, "event: done\ndata: {}\n\n")
				flush()
				return
			}
		case <-sess.Done:
			fmt.Fprint(w, "event: done\ndata: {}\n\n")
			flush()
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleClip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.Error(w, "bad clip index", http.StatusBadRequest)
		return
	}
	sess, ok := s.sessions.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	clip, ok := sess.Clip(n)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(clip.Data)))
	w.Write(clip.Data) //nolint:errcheck
}
