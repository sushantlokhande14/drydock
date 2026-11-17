// Package api is the HTTP surface of the control plane: submit and inspect
// runs, and stream what they are doing as server-sent events.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/sushantlokhande14/drydock/internal/events"
	"github.com/sushantlokhande14/drydock/internal/orchestrator"
	"github.com/sushantlokhande14/drydock/internal/tasks"
)

// Server wires the orchestrator and the event store to HTTP handlers.
type Server struct {
	Orchestrator *orchestrator.Orchestrator
	Store        *events.Store
	// Tasks are the bundled example tasks, already imported into the repository cache.
	Tasks []tasks.Loaded
	// UI serves the control plane's static files.
	UI http.Handler
	// DefaultModel is what a bundled task runs with when the request does not say.
	DefaultModel string
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Orchestrator.Stats()) })
	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Orchestrator.List()) })
	mux.HandleFunc("POST /api/runs", s.submit)
	mux.HandleFunc("GET /api/runs/{id}", s.get)
	mux.HandleFunc("POST /api/runs/{id}/cancel", s.cancel)
	mux.HandleFunc("GET /api/runs/{id}/events", s.runEvents)
	mux.HandleFunc("GET /api/fleet", s.fleet)
	mux.HandleFunc("GET /api/tasks", s.listTasks)
	mux.HandleFunc("POST /api/tasks/{name}/runs", s.runTask)
	if s.UI != nil {
		mux.Handle("/", s.UI)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) accept(w http.ResponseWriter, spec orchestrator.TaskSpec) {
	summary, err := s.Orchestrator.Submit(spec)
	switch {
	case errors.Is(err, orchestrator.ErrBusy):
		// Refuse rather than queue without bound, and say when to come back.
		w.Header().Set("Retry-After", "2")
		writeError(w, http.StatusTooManyRequests, "the queue is full; retry shortly")
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusAccepted, summary)
	}
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request) {
	var spec orchestrator.TaskSpec
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	s.accept(w, spec)
}

func (s *Server) listTasks(w http.ResponseWriter, _ *http.Request) {
	type item struct {
		Name  string `json:"name"`
		Title string `json:"title"`
		Issue string `json:"issue"`
	}
	out := make([]item, 0, len(s.Tasks))
	for _, task := range s.Tasks {
		out = append(out, item{Name: task.Name, Title: task.Title, Issue: task.Issue})
	}
	writeJSON(w, http.StatusOK, out)
}

// runTask starts one or more runs of a bundled task. The body may name a model
// and a count; "scripted" replays the task's recorded solution.
func (s *Server) runTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model string `json:"model"`
		Count int    `json:"count"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}
	if body.Model == "" {
		body.Model = s.DefaultModel
	}
	if body.Count <= 0 {
		body.Count = 1
	}
	if body.Count > 512 {
		writeError(w, http.StatusBadRequest, "count must be at most 512")
		return
	}
	name := r.PathValue("name")
	for _, task := range s.Tasks {
		if task.Name != name {
			continue
		}
		started := make([]orchestrator.Summary, 0, body.Count)
		for i := 0; i < body.Count; i++ {
			summary, err := s.Orchestrator.Submit(task.Spec(body.Model))
			if errors.Is(err, orchestrator.ErrBusy) {
				break
			}
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			started = append(started, summary)
		}
		if len(started) == 0 {
			w.Header().Set("Retry-After", "2")
			writeError(w, http.StatusTooManyRequests, "the queue is full; retry shortly")
			return
		}
		writeJSON(w, http.StatusAccepted, started)
		return
	}
	writeError(w, http.StatusNotFound, "no such task")
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	summary, patch, err := s.Orchestrator.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": summary, "patch": patch})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if err := s.Orchestrator.Cancel(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stream prepares a response for server-sent events and returns a function
// that writes one event. It returns nil if the connection cannot stream.
func stream(w http.ResponseWriter) func(id, event string, data []byte) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming is not supported on this connection")
		return nil
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	// Tell reverse proxies not to buffer: a buffered event stream is not live.
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return func(id, event string, data []byte) error {
		var err error
		if id != "" {
			_, err = fmt.Fprintf(w, "id: %s\n", id)
		}
		if err == nil && event != "" {
			_, err = fmt.Fprintf(w, "event: %s\n", event)
		}
		if err == nil {
			_, err = fmt.Fprintf(w, "data: %s\n\n", data)
		}
		if err == nil {
			flusher.Flush()
		}
		return err
	}
}

// runEvents streams one run's log. A client that reconnects sends the last
// sequence number it received (browsers do this on their own through the
// Last-Event-ID header) and continues from the next event, so a dropped
// connection never loses or repeats anything.
func (s *Server) runEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	after := int64(0)
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		after, _ = strconv.ParseInt(value, 10, 64)
	} else if value := r.URL.Query().Get("after"); value != "" {
		after, _ = strconv.ParseInt(value, 10, 64)
	}
	feed, cancel, err := s.Store.Subscribe(id, after)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such run")
		return
	}
	defer cancel()
	send := stream(w)
	if send == nil {
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			// A comment line keeps idle connections from being closed by intermediaries.
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		case event, open := <-feed:
			if !open {
				// Either the run is over or this reader fell too far behind. In both cases the
				// client decides: "end" tells it not to reconnect when the run has finished.
				if summary, _, err := s.Orchestrator.Get(id); err == nil && summary.State.Terminal() {
					_ = send("", "end", []byte(`{}`))
				}
				return
			}
			payload, _ := json.Marshal(event)
			if send(strconv.FormatInt(event.Seq, 10), "", payload) != nil {
				return
			}
		}
	}
}

// fleet streams every run's summary: a snapshot first, then changes. Runs
// emit far more events than a grid of tiles needs, so changes are collected
// and sent a few times a second, each run at most once per batch.
func (s *Server) fleet(w http.ResponseWriter, r *http.Request) {
	feed, cancel := s.Store.SubscribeAll()
	defer cancel()
	send := stream(w)
	if send == nil {
		return
	}
	snapshot, _ := json.Marshal(s.Orchestrator.List())
	if send("", "snapshot", snapshot) != nil {
		return
	}

	var mu sync.Mutex
	dirty := map[string]struct{}{}
	ctx, stop := context.WithCancel(r.Context())
	defer stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case event, open := <-feed:
				if !open {
					// Dropped for being slow. Ending the response makes the browser reconnect
					// and start again from a fresh snapshot.
					stop()
					return
				}
				mu.Lock()
				dirty[event.Run] = struct{}{}
				mu.Unlock()
			}
		}
	}()

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			mu.Lock()
			changed := dirty
			dirty = map[string]struct{}{}
			mu.Unlock()
			if len(changed) == 0 {
				continue
			}
			batch := make([]orchestrator.Summary, 0, len(changed))
			for id := range changed {
				if summary, _, err := s.Orchestrator.Get(id); err == nil {
					batch = append(batch, summary)
				}
			}
			payload, _ := json.Marshal(struct {
				Runs  []orchestrator.Summary `json:"runs"`
				Stats orchestrator.Stats     `json:"stats"`
			}{batch, s.Orchestrator.Stats()})
			if send("", "runs", payload) != nil {
				return
			}
		}
	}
}
