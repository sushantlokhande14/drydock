// Package events is the system's source of truth: an append-only log per run.
//
// Everything a run does is written here first, as an ordered event: a state
// change, a request to the model, a tool call, a chunk of terminal output, a
// patch. Nothing else is stored. A run's current state is a fold over its
// log, the live UI is a subscription to it, a reconnecting browser resumes
// from the last sequence number it saw, and a restarted server rebuilds its
// view of every run by reading the files back.
package events

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Event types. Data is a JSON object whose shape depends on the type; the
// structs below document each one.
const (
	RunCreated    = "run.created"
	RunState      = "run.state"
	SandboxReady  = "sandbox.ready"
	RepoReady     = "repo.ready"
	ModelRequest  = "model.request"
	ModelResponse = "model.response"
	ToolCall      = "tool.call"
	ToolOutput    = "tool.output"
	ToolResult    = "tool.result"
	PatchCreated  = "patch.created"
	VerifyResult  = "verify.result"
	RunFinished   = "run.finished"
)

// Event is one entry in a run's log.
type Event struct {
	// Seq numbers a run's events from 1 with no gaps. It is the resume cursor.
	Seq  int64           `json:"seq"`
	Run  string          `json:"run"`
	Time time.Time       `json:"time"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Store appends events to one file per run and fans them out to subscribers.
type Store struct {
	dir string

	mu   sync.Mutex
	runs map[string]*runLog
	// all receives every event of every run, for views that span runs.
	all map[chan Event]struct{}
}

type runLog struct {
	mu     sync.Mutex
	file   *os.File
	events []Event
	subs   map[chan Event]struct{}
	closed bool
}

// Open returns a store rooted at dir, creating it if needed. An empty dir
// keeps events in memory only, which is what tests and benchmarks use.
func Open(dir string) (*Store, error) {
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir, runs: map[string]*runLog{}, all: map[chan Event]struct{}{}}, nil
}

func (s *Store) log(run string, create bool) (*runLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.runs[run]; ok {
		return existing, nil
	}
	if !create {
		return nil, ErrUnknownRun
	}
	created := &runLog{subs: map[chan Event]struct{}{}}
	if s.dir != "" {
		file, err := os.OpenFile(s.path(run), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		created.file = file
	}
	s.runs[run] = created
	return created, nil
}

func (s *Store) path(run string) string { return filepath.Join(s.dir, run+".jsonl") }

// ErrUnknownRun means no events exist for the run.
var ErrUnknownRun = errors.New("events: unknown run")

// Append writes an event to the run's log and delivers it to subscribers.
// The sequence number is assigned here, under the run's lock, so two
// goroutines of one run can never produce the same number or a gap.
func (s *Store) Append(run, eventType string, data any) (Event, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return Event{}, fmt.Errorf("events: encoding %s: %w", eventType, err)
	}
	log, err := s.log(run, true)
	if err != nil {
		return Event{}, err
	}

	log.mu.Lock()
	event := Event{
		Seq:  int64(len(log.events)) + 1,
		Run:  run,
		Time: time.Now().UTC(),
		Type: eventType,
		Data: encoded,
	}
	if log.file != nil {
		line, _ := json.Marshal(event)
		if _, err := log.file.Write(append(line, '\n')); err != nil {
			log.mu.Unlock()
			return Event{}, fmt.Errorf("events: writing log: %w", err)
		}
	}
	log.events = append(log.events, event)
	// Deliver while still holding the run's lock, so every subscriber sees this run's
	// events in sequence order. Lock order is always run log, then store.
	for sub := range log.subs {
		if !deliver(sub, event) {
			delete(log.subs, sub)
		}
	}
	s.mu.Lock()
	for sub := range s.all {
		if !deliver(sub, event) {
			delete(s.all, sub)
		}
	}
	s.mu.Unlock()
	log.mu.Unlock()
	return event, nil
}

// deliver never blocks the writer. A subscriber that has fallen a full buffer
// behind is dropped: its channel is closed and it is forgotten. It reconnects
// with the last sequence number it saw and replays what it missed from the
// log, so a slow reader loses time, never events.
func deliver(sub chan Event, event Event) bool {
	select {
	case sub <- event:
		return true
	default:
		close(sub)
		return false
	}
}

// Subscribe returns the run's events after sequence number `after`, first the
// ones already logged and then new ones as they are appended. The channel is
// closed when the run's log is closed, when the subscriber falls too far
// behind, or when cancel is called.
func (s *Store) Subscribe(run string, after int64) (<-chan Event, func(), error) {
	log, err := s.log(run, false)
	if err != nil {
		return nil, nil, err
	}
	log.mu.Lock()
	defer log.mu.Unlock()

	backlog := 0
	if after < int64(len(log.events)) {
		backlog = len(log.events) - int(max(after, 0))
	}
	sub := make(chan Event, backlog+subscriberBuffer)
	for _, event := range log.events {
		if event.Seq > after {
			sub <- event
		}
	}
	if log.closed {
		close(sub)
		return sub, func() {}, nil
	}
	log.subs[sub] = struct{}{}
	cancel := func() {
		log.mu.Lock()
		defer log.mu.Unlock()
		if _, ok := log.subs[sub]; ok {
			delete(log.subs, sub)
			safeClose(sub)
		}
	}
	return sub, cancel, nil
}

const subscriberBuffer = 1024

func safeClose(ch chan Event) {
	defer func() { _ = recover() }()
	close(ch)
}

// SubscribeAll returns every event appended from now on, across all runs.
func (s *Store) SubscribeAll() (<-chan Event, func()) {
	sub := make(chan Event, 4*subscriberBuffer)
	s.mu.Lock()
	s.all[sub] = struct{}{}
	s.mu.Unlock()
	return sub, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if _, ok := s.all[sub]; ok {
			delete(s.all, sub)
			safeClose(sub)
		}
	}
}

// Events returns a copy of everything logged for the run so far.
func (s *Store) Events(run string) ([]Event, error) {
	log, err := s.log(run, false)
	if err != nil {
		return nil, err
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	return append([]Event(nil), log.events...), nil
}

// Close marks the run's log as complete: the file is closed and subscribers'
// channels end after the last event. Appending to a closed log is an error.
func (s *Store) Close(run string) {
	log, err := s.log(run, false)
	if err != nil {
		return
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.closed {
		return
	}
	log.closed = true
	if log.file != nil {
		_ = log.file.Close()
		log.file = nil
	}
	for sub := range log.subs {
		safeClose(sub)
	}
	log.subs = map[chan Event]struct{}{}
}

// Runs lists the ids of every run the store knows, oldest first by id.
func (s *Store) Runs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Load reads every log file in the store's directory back into memory. It is
// called once at start-up. Logs loaded this way are closed: a run that was in
// flight when the process died is over, and its log says how far it got.
//
// A final line cut short by a crash is skipped rather than treated as corruption.
func (s *Store) Load() (int, error) {
	if s.dir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	loaded := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		run := strings.TrimSuffix(name, ".jsonl")
		file, err := os.Open(filepath.Join(s.dir, name))
		if err != nil {
			return loaded, err
		}
		log := &runLog{subs: map[chan Event]struct{}{}, closed: true}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
		for scanner.Scan() {
			var event Event
			if json.Unmarshal(scanner.Bytes(), &event) != nil {
				break
			}
			// Sequence numbers must be contiguous; stop at the first break rather than guess.
			if event.Seq != int64(len(log.events))+1 {
				break
			}
			log.events = append(log.events, event)
		}
		_ = file.Close()
		if len(log.events) == 0 {
			continue
		}
		s.mu.Lock()
		s.runs[run] = log
		s.mu.Unlock()
		loaded++
	}
	return loaded, nil
}

// CloseAll closes every run's log. It is called when the process shuts down.
func (s *Store) CloseAll() {
	for _, run := range s.Runs() {
		s.Close(run)
	}
}
