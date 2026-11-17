package orchestrator

import (
	"encoding/json"
	"time"

	"github.com/sushantlokhande14/drydock/internal/events"
	"github.com/sushantlokhande14/drydock/internal/model"
)

// State is where a run is in its life.
type State string

const (
	StateQueued       State = "queued"
	StateProvisioning State = "provisioning"
	StateRunning      State = "running"
	StateVerifying    State = "verifying"
	StateSucceeded    State = "succeeded"
	StateFailed       State = "failed"
	StateCancelled    State = "cancelled"
)

// Terminal reports whether the state is final.
func (s State) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// TaskSpec is a request to run one agent on one repository.
type TaskSpec struct {
	Title string `json:"title"`
	Issue string `json:"issue"`

	// Exactly one of RepoURL and Mirror names the repository. RepoURL is an https
	// clone URL fetched through the cache. Mirror is the path of a mirror that is
	// already in the cache, which is how local task repositories are referenced.
	RepoURL string `json:"repoUrl,omitempty"`
	Mirror  string `json:"mirror,omitempty"`
	Ref     string `json:"ref,omitempty"`

	// TestCommand is what the agent's run_tests tool runs.
	TestCommand string `json:"testCommand"`
	// VerifyCommand decides success. It runs in a fresh sandbox, on a clean checkout
	// with only the patch applied. It defaults to TestCommand.
	VerifyCommand string `json:"verifyCommand,omitempty"`
	// VerifyFiles are added before verification and are never shown to the agent:
	// hidden tests, in the sense of the SWE-bench protocol.
	VerifyFiles map[string]string `json:"verifyFiles,omitempty"`

	// Model selects the backend: "scripted", or "openai:<model name>".
	Model string `json:"model"`
	// Script is the recording a scripted run replays.
	Script *model.Script `json:"script,omitempty"`

	MaxSteps int `json:"maxSteps,omitempty"`
}

// Timing records where a run's wall-clock time went, in milliseconds.
type Timing struct {
	Queue int64 `json:"queueMs"`
	// Sandbox is the time to obtain a started sandbox, from the pool or by creating one.
	Sandbox int64 `json:"sandboxMs"`
	// Checkout is the time to put the repository's working tree into the sandbox.
	Checkout int64 `json:"checkoutMs"`
	// Startup is Sandbox plus Checkout: the time from a worker picking the run up
	// to the first moment a command can run against the repository.
	Startup int64 `json:"startupMs"`
	Agent   int64 `json:"agentMs"`
	Verify  int64 `json:"verifyMs"`
	Total   int64 `json:"totalMs"`
}

// PatchStats summarises the change an agent produced.
type PatchStats struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// Summary is the current state of a run. It is derived entirely from the
// run's events by Apply, so the live view and the view rebuilt from disk
// after a restart cannot disagree.
type Summary struct {
	ID         string      `json:"id"`
	Title      string      `json:"title"`
	Repo       string      `json:"repo"`
	Model      string      `json:"model"`
	State      State       `json:"state"`
	Phase      string      `json:"phase"`
	Step       int         `json:"step"`
	ToolCalls  int         `json:"toolCalls"`
	LastTool   string      `json:"lastTool,omitempty"`
	WarmStart  bool        `json:"warmStart"`
	Verified   bool        `json:"verified"`
	Reason     string      `json:"reason,omitempty"`
	Error      string      `json:"error,omitempty"`
	Patch      *PatchStats `json:"patch,omitempty"`
	Timing     Timing      `json:"timing"`
	CreatedAt  time.Time   `json:"createdAt"`
	FinishedAt *time.Time  `json:"finishedAt,omitempty"`
	Events     int64       `json:"events"`
}

// Event payloads written by the orchestrator. The agent package defines its own.
type (
	runCreatedEvent struct {
		Title string `json:"title"`
		Issue string `json:"issue"`
		Repo  string `json:"repo"`
		Ref   string `json:"ref"`
		Model string `json:"model"`
	}
	runStateEvent struct {
		State State  `json:"state"`
		Phase string `json:"phase,omitempty"`
	}
	sandboxReadyEvent struct {
		Sandbox string `json:"sandbox"`
		Warm    bool   `json:"warm"`
		Millis  int64  `json:"ms"`
		QueueMs int64  `json:"queueMs"`
	}
	repoReadyEvent struct {
		Commit        string `json:"commit"`
		CheckoutMs    int64  `json:"checkoutMs"`
		StartupMs     int64  `json:"startupMs"`
		MirrorFetched bool   `json:"mirrorFetched"`
		Files         int    `json:"files"`
	}
	patchCreatedEvent struct {
		Diff  string     `json:"diff"`
		Stats PatchStats `json:"stats"`
	}
	verifyResultEvent struct {
		Passed  bool   `json:"passed"`
		Output  string `json:"output"`
		Millis  int64  `json:"ms"`
		Sandbox string `json:"sandbox"`
	}
	runFinishedEvent struct {
		State    State  `json:"state"`
		Reason   string `json:"reason"`
		Error    string `json:"error,omitempty"`
		Summary  string `json:"summary,omitempty"`
		Verified bool   `json:"verified"`
		Timing   Timing `json:"timing"`
	}
)

// Reasons a run ends without success.
const (
	ReasonVerified        = "patch_verified"
	ReasonNoChange        = "no_change_made"
	ReasonVerifyFailed    = "verification_failed"
	ReasonCancelled       = "cancelled"
	ReasonHarnessError    = "harness_error"
	ReasonInterrupted     = "interrupted_by_restart"
	ReasonBudgetNoSubmit  = "step_budget_exhausted"
	ReasonModelStoppedOut = "model_stopped_calling_tools"
)

// Apply folds one event into the summary. Unknown event types change nothing
// but the event count, so old code can read logs written by newer code.
func (s *Summary) Apply(event events.Event) {
	s.Events = event.Seq
	switch event.Type {
	case events.RunCreated:
		var data runCreatedEvent
		if json.Unmarshal(event.Data, &data) == nil {
			s.ID = event.Run
			s.Title = data.Title
			s.Repo = data.Repo
			s.Model = data.Model
			s.State = StateQueued
			s.CreatedAt = event.Time
		}
	case events.RunState:
		var data runStateEvent
		if json.Unmarshal(event.Data, &data) == nil {
			s.State = data.State
			if data.Phase != "" {
				s.Phase = data.Phase
			}
		}
	case events.SandboxReady:
		var data sandboxReadyEvent
		if json.Unmarshal(event.Data, &data) == nil {
			s.WarmStart = data.Warm
			s.Timing.Sandbox = data.Millis
			s.Timing.Queue = data.QueueMs
		}
	case events.RepoReady:
		var data repoReadyEvent
		if json.Unmarshal(event.Data, &data) == nil {
			s.Timing.Checkout = data.CheckoutMs
			s.Timing.Startup = data.StartupMs
		}
	case events.ModelRequest:
		var data struct {
			Step int `json:"step"`
		}
		if json.Unmarshal(event.Data, &data) == nil {
			s.Step = data.Step
		}
	case events.ToolCall:
		var data struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(event.Data, &data) == nil {
			s.ToolCalls++
			s.LastTool = data.Name
			s.Phase = phaseOf(data.Name, s.Phase)
		}
	case events.PatchCreated:
		var data patchCreatedEvent
		if json.Unmarshal(event.Data, &data) == nil {
			stats := data.Stats
			s.Patch = &stats
		}
	case events.VerifyResult:
		var data verifyResultEvent
		if json.Unmarshal(event.Data, &data) == nil {
			s.Verified = data.Passed
			s.Timing.Verify = data.Millis
		}
	case events.RunFinished:
		var data runFinishedEvent
		if json.Unmarshal(event.Data, &data) == nil {
			s.State = data.State
			s.Reason = data.Reason
			s.Error = data.Error
			s.Verified = data.Verified
			s.Timing = data.Timing
			finished := event.Time
			s.FinishedAt = &finished
		}
	}
}

// phaseOf names the part of the work a tool belongs to, for the fleet view:
// reading is analysis, writing is editing, running is testing.
func phaseOf(tool, current string) string {
	switch tool {
	case "list_files", "read_file", "search":
		if current == "" || current == "analysis" {
			return "analysis"
		}
		return current
	case "replace_in_file", "write_file":
		return "editing"
	case "run_tests", "run_command":
		return "testing"
	case "submit":
		return "delivering"
	}
	return current
}

// Fold rebuilds a summary from a complete event log. A log that ends without
// a run.finished event belongs to a run that was in flight when the process
// stopped; it is reported as failed, with that reason, rather than left
// looking as if it were still running.
func Fold(log []events.Event) Summary {
	var summary Summary
	for _, event := range log {
		summary.Apply(event)
	}
	if !summary.State.Terminal() && len(log) > 0 {
		summary.State = StateFailed
		summary.Reason = ReasonInterrupted
		last := log[len(log)-1].Time
		summary.FinishedAt = &last
	}
	return summary
}
