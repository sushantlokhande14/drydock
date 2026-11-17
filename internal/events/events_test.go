package events

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func collect(t *testing.T, feed <-chan Event, want int) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(5 * time.Second)
	for len(got) < want {
		select {
		case event, open := <-feed:
			if !open {
				t.Fatalf("stream ended after %d of %d events", len(got), want)
			}
			got = append(got, event)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events", len(got), want)
		}
	}
	return got
}

func TestSequenceNumbersAreContiguousUnderConcurrentWriters(t *testing.T) {
	store, _ := Open("")
	const writers, each = 16, 200
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := store.Append("r1", ToolOutput, map[string]int{"i": i}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	log, err := store.Events("r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != writers*each {
		t.Fatalf("logged %d events, want %d", len(log), writers*each)
	}
	for index, event := range log {
		if event.Seq != int64(index)+1 {
			t.Fatalf("event %d has sequence number %d", index, event.Seq)
		}
	}
}

func TestSubscribeReplaysThenFollows(t *testing.T) {
	store, _ := Open("")
	store.Append("r1", RunCreated, nil)
	store.Append("r1", RunState, nil)

	feed, cancel, err := store.Subscribe("r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	store.Append("r1", ToolCall, nil)
	store.Append("r1", ToolResult, nil)

	got := collect(t, feed, 4)
	for index, event := range got {
		if event.Seq != int64(index)+1 {
			t.Fatalf("delivered out of order: %v", got)
		}
	}
	if got[0].Type != RunCreated || got[3].Type != ToolResult {
		t.Fatalf("unexpected types: %v", got)
	}
}

func TestSubscribeResumesAfterASequenceNumber(t *testing.T) {
	store, _ := Open("")
	for i := 0; i < 10; i++ {
		store.Append("r1", ToolOutput, nil)
	}
	// A client that saw events 1..7 reconnects.
	feed, cancel, _ := store.Subscribe("r1", 7)
	defer cancel()
	store.Append("r1", RunFinished, nil)

	got := collect(t, feed, 4)
	if got[0].Seq != 8 || got[3].Seq != 11 {
		t.Fatalf("resume delivered %d..%d, want 8..11", got[0].Seq, got[3].Seq)
	}
}

func TestSubscribersOfOneRunDoNotSeeAnother(t *testing.T) {
	store, _ := Open("")
	store.Append("a", RunCreated, nil)
	store.Append("b", RunCreated, nil)
	feed, cancel, _ := store.Subscribe("a", 0)
	defer cancel()
	store.Append("b", RunState, nil)
	store.Append("a", RunState, nil)

	for _, event := range collect(t, feed, 2) {
		if event.Run != "a" {
			t.Fatalf("received an event of run %q", event.Run)
		}
	}
	if _, _, err := store.Subscribe("missing", 0); err != ErrUnknownRun {
		t.Fatalf("subscribing to an unknown run: %v", err)
	}
}

func TestCloseEndsTheStreamAfterTheLastEvent(t *testing.T) {
	store, _ := Open("")
	store.Append("r1", RunCreated, nil)
	feed, cancel, _ := store.Subscribe("r1", 0)
	defer cancel()
	store.Append("r1", RunFinished, nil)
	store.Close("r1")

	got := collect(t, feed, 2)
	if got[1].Type != RunFinished {
		t.Fatalf("last event was %s", got[1].Type)
	}
	if _, open := <-feed; open {
		t.Fatal("the stream should end when the log is closed")
	}
	// A late subscriber still gets the whole log, then the end.
	late, cancelLate, _ := store.Subscribe("r1", 0)
	defer cancelLate()
	collect(t, late, 2)
	if _, open := <-late; open {
		t.Fatal("a subscription to a closed log should end after the backlog")
	}
}

func TestASlowSubscriberIsDroppedAndCanResumeWithoutLoss(t *testing.T) {
	store, _ := Open("")
	store.Append("r1", RunCreated, nil)
	slow, cancel, _ := store.Subscribe("r1", 0)
	defer cancel()

	// Nobody reads `slow`. Write more than its buffer holds; the writer must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberBuffer*2; i++ {
			store.Append("r1", ToolOutput, nil)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a subscriber that does not read blocked the writer")
	}

	last := int64(0)
	for event := range slow {
		if event.Seq != last+1 {
			t.Fatalf("gap before the drop: %d then %d", last, event.Seq)
		}
		last = event.Seq
	}
	total := int64(subscriberBuffer*2 + 1)
	if last >= total {
		t.Fatalf("the slow subscriber was never dropped (received all %d)", last)
	}

	// It reconnects from where it stopped and receives exactly the rest.
	resumed, cancelResumed, _ := store.Subscribe("r1", last)
	defer cancelResumed()
	rest := collect(t, resumed, int(total-last))
	if rest[0].Seq != last+1 || rest[len(rest)-1].Seq != total {
		t.Fatalf("resume covered %d..%d, want %d..%d", rest[0].Seq, rest[len(rest)-1].Seq, last+1, total)
	}
}

func TestSubscribeAllSeesEveryRun(t *testing.T) {
	store, _ := Open("")
	feed, cancel := store.SubscribeAll()
	defer cancel()
	store.Append("a", RunCreated, nil)
	store.Append("b", RunCreated, nil)
	store.Append("a", RunState, nil)

	got := collect(t, feed, 3)
	if got[0].Run != "a" || got[1].Run != "b" || got[2].Run != "a" || got[2].Seq != 2 {
		t.Fatalf("unexpected events: %+v", got)
	}
}

func TestLogsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.Append("r1", RunCreated, map[string]string{"title": "fix it"})
	first.Append("r1", ToolCall, map[string]string{"name": "read_file"})
	first.Append("r2", RunCreated, nil)
	// The process stops. Whether a log was closed cleanly or not, it must load.
	first.CloseAll()

	second, _ := Open(dir)
	loaded, err := second.Load()
	if err != nil || loaded != 2 {
		t.Fatalf("loaded %d logs, err %v", loaded, err)
	}
	log, _ := second.Events("r1")
	if len(log) != 2 || log[1].Type != ToolCall || string(log[0].Data) != `{"title":"fix it"}` {
		t.Fatalf("reloaded log differs: %+v", log)
	}
	// A reloaded log is complete: subscribing replays it and ends.
	feed, cancel, _ := second.Subscribe("r2", 0)
	defer cancel()
	collect(t, feed, 1)
	if _, open := <-feed; open {
		t.Fatal("a reloaded log should be closed")
	}
}

func TestATornFinalLineIsIgnoredOnLoad(t *testing.T) {
	dir := t.TempDir()
	store, _ := Open(dir)
	store.Append("r1", RunCreated, nil)
	store.Append("r1", RunState, nil)
	store.Close("r1")

	// The process died while writing the third event.
	file, err := os.OpenFile(filepath.Join(dir, "r1.jsonl"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(`{"seq":3,"run":"r1","type":"tool.ca`)
	file.Close()

	reopened, _ := Open(dir)
	if _, err := reopened.Load(); err != nil {
		t.Fatal(err)
	}
	log, _ := reopened.Events("r1")
	if len(log) != 2 {
		t.Fatalf("kept %d events, want the 2 complete ones", len(log))
	}
}

func TestAGapInSequenceNumbersStopsTheLoad(t *testing.T) {
	dir := t.TempDir()
	lines := `{"seq":1,"run":"r1","type":"run.created","data":{}}
{"seq":2,"run":"r1","type":"run.state","data":{}}
{"seq":4,"run":"r1","type":"run.finished","data":{}}
`
	if err := os.WriteFile(filepath.Join(dir, "r1.jsonl"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	store, _ := Open(dir)
	store.Load()
	log, _ := store.Events("r1")
	if len(log) != 2 {
		t.Fatalf("kept %d events; events after a gap cannot be trusted", len(log))
	}
}
