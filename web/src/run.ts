import type { RunEvent, RunState, Timing } from "./types";

// A run's view is a fold over its event log, the same way the server derives
// its summary. The UI holds no state the log does not contain, so a page that
// is opened late, or reconnects, ends up identical to one that watched live.

export interface ToolCallView {
  id: string;
  name: string;
  args: Record<string, unknown>;
  /** Terminal output streamed while the tool ran. */
  output: string;
  /** What was returned to the model, once the tool finished. */
  result?: string;
  isError?: boolean;
  exitCode?: number;
  durationMs?: number;
}

export interface StepView {
  step: number;
  reasoning: string;
  modelMs?: number;
  calls: ToolCallView[];
}

export interface Milestone {
  seq: number;
  time: string;
  kind: "state" | "sandbox" | "repo" | "patch" | "verify" | "finished";
  text: string;
  detail?: string;
  tone?: "good" | "bad" | "muted";
}

export interface RunView {
  title: string;
  issue: string;
  repo: string;
  model: string;
  state: RunState;
  phase: string;
  steps: StepView[];
  /** Milestones and steps in the order they happened; steps are referenced by index. */
  timeline: Array<{ milestone: Milestone } | { stepIndex: number }>;
  diff: string;
  patchStats?: { files: number; additions: number; deletions: number };
  verify?: { passed: boolean; output: string; ms: number };
  reason?: string;
  error?: string;
  summary?: string;
  timing?: Timing;
  warm?: boolean;
  sandbox?: string;
  lastSeq: number;
}

export function emptyRun(): RunView {
  return {
    title: "",
    issue: "",
    repo: "",
    model: "",
    state: "queued",
    phase: "",
    steps: [],
    timeline: [],
    diff: "",
    lastSeq: 0,
  };
}

const text = (value: unknown): string => (typeof value === "string" ? value : "");
const number = (value: unknown): number => (typeof value === "number" ? value : 0);

/** Applies one event. Returns a new object so React sees the change; events are applied in order, once. */
export function fold(view: RunView, event: RunEvent): RunView {
  if (event.seq <= view.lastSeq) return view; // a replayed event after a reconnect
  const next: RunView = { ...view, lastSeq: event.seq };
  const data = event.data ?? {};
  const milestone = (m: Omit<Milestone, "seq" | "time">) => {
    next.timeline = [...next.timeline, { milestone: { ...m, seq: event.seq, time: event.time } }];
  };
  const stepFor = (step: number): StepView => {
    let index = next.steps.findIndex((s) => s.step === step);
    if (index < 0) {
      next.steps = [...next.steps, { step, reasoning: "", calls: [] }];
      index = next.steps.length - 1;
      next.timeline = [...next.timeline, { stepIndex: index }];
    } else {
      next.steps = [...next.steps];
    }
    next.steps[index] = { ...next.steps[index], calls: [...next.steps[index].calls] };
    return next.steps[index];
  };
  const findCall = (id: string): ToolCallView | undefined => {
    for (let i = next.steps.length - 1; i >= 0; i--) {
      const position = next.steps[i].calls.findIndex((c) => c.id === id);
      if (position >= 0) {
        next.steps = [...next.steps];
        const calls = [...next.steps[i].calls];
        calls[position] = { ...calls[position] };
        next.steps[i] = { ...next.steps[i], calls };
        return calls[position];
      }
    }
    return undefined;
  };

  switch (event.type) {
    case "run.created":
      next.title = text(data.title);
      next.issue = text(data.issue);
      next.repo = text(data.repo);
      next.model = text(data.model);
      break;
    case "run.state":
      next.state = text(data.state) as RunState;
      if (data.phase) next.phase = text(data.phase);
      milestone({ kind: "state", text: stateLabel(next.state), tone: "muted" });
      break;
    case "sandbox.ready":
      next.warm = Boolean(data.warm);
      next.sandbox = text(data.sandbox);
      milestone({
        kind: "sandbox",
        text: `Sandbox ${text(data.sandbox)} ready in ${number(data.ms)} ms`,
        detail: next.warm ? "taken from the warm pool" : "created on demand",
      });
      break;
    case "repo.ready":
      milestone({
        kind: "repo",
        text: `Repository checked out at ${text(data.commit).slice(0, 10)}`,
        detail: `${number(data.files)} files, startup ${number(data.startupMs)} ms${data.mirrorFetched ? ", mirror fetched" : ""}`,
      });
      break;
    case "model.response": {
      const step = stepFor(number(data.step));
      step.reasoning = text(data.reasoning);
      step.modelMs = number(data.durationMs);
      break;
    }
    case "tool.call": {
      const step = stepFor(number(data.step));
      step.calls.push({
        id: text(data.id),
        name: text(data.name),
        args: (data.args as Record<string, unknown>) ?? {},
        output: "",
      });
      next.phase = phaseOf(text(data.name), next.phase);
      break;
    }
    case "tool.output": {
      const call = findCall(text(data.id));
      if (call) call.output += text(data.text);
      break;
    }
    case "tool.result": {
      const call = findCall(text(data.id));
      if (call) {
        call.result = text(data.output);
        call.isError = Boolean(data.isError);
        call.exitCode = typeof data.exitCode === "number" ? data.exitCode : undefined;
        call.durationMs = number(data.durationMs);
      }
      break;
    }
    case "patch.created": {
      next.diff = text(data.diff);
      const stats = data.stats as RunView["patchStats"];
      next.patchStats = stats;
      milestone({
        kind: "patch",
        text: "Patch collected",
        detail: stats ? `${stats.files} file${stats.files === 1 ? "" : "s"}, +${stats.additions} -${stats.deletions}` : undefined,
      });
      break;
    }
    case "verify.result":
      next.verify = { passed: Boolean(data.passed), output: text(data.output), ms: number(data.ms) };
      milestone({
        kind: "verify",
        text: data.passed ? "Verified on a clean checkout" : "Failed verification on a clean checkout",
        detail: `second sandbox ${text(data.sandbox)}, ${number(data.ms)} ms`,
        tone: data.passed ? "good" : "bad",
      });
      break;
    case "run.finished":
      next.state = text(data.state) as RunState;
      next.reason = text(data.reason);
      next.error = text(data.error) || undefined;
      next.summary = text(data.summary) || undefined;
      next.timing = data.timing as Timing;
      milestone({
        kind: "finished",
        text: `${stateLabel(next.state)}: ${reasonLabel(next.reason)}`,
        detail: next.error,
        tone: next.state === "succeeded" ? "good" : next.state === "cancelled" ? "muted" : "bad",
      });
      break;
  }
  return next;
}

function phaseOf(tool: string, current: string): string {
  switch (tool) {
    case "list_files":
    case "read_file":
    case "search":
      return current === "" || current === "analysis" ? "analysis" : current;
    case "replace_in_file":
    case "write_file":
      return "editing";
    case "run_tests":
    case "run_command":
      return "testing";
    case "submit":
      return "delivering";
  }
  return current;
}

export function stateLabel(state: RunState): string {
  return {
    queued: "Queued",
    provisioning: "Provisioning",
    running: "Running",
    verifying: "Verifying",
    succeeded: "Succeeded",
    failed: "Failed",
    cancelled: "Cancelled",
  }[state];
}

export function reasonLabel(reason?: string): string {
  switch (reason) {
    case "patch_verified":
      return "the patch passed the hidden tests on a clean checkout";
    case "no_change_made":
      return "the agent submitted without changing any file";
    case "verification_failed":
      return "the patch did not pass the hidden tests";
    case "step_budget_exhausted":
      return "the step budget ran out";
    case "model_stopped_calling_tools":
      return "the model stopped calling tools";
    case "cancelled":
      return "cancelled";
    case "harness_error":
      return "the harness hit an error";
    case "interrupted_by_restart":
      return "the server stopped while it was running";
    default:
      return reason ?? "";
  }
}

/** A one-line description of a tool call, for the timeline. */
export function describeCall(call: ToolCallView): string {
  const arg = (key: string) => text(call.args[key]);
  switch (call.name) {
    case "list_files":
      return arg("path") || "whole repository";
    case "read_file": {
      const start = call.args.start_line;
      return arg("path") + (typeof start === "number" && start > 1 ? ` from line ${start}` : "");
    }
    case "search":
      return `/${arg("pattern")}/` + (arg("path") ? ` in ${arg("path")}` : "");
    case "replace_in_file":
    case "write_file":
      return arg("path");
    case "run_command":
      return arg("command");
    case "run_tests":
      return "project test suite";
    case "submit":
      return arg("summary");
    default:
      return JSON.stringify(call.args);
  }
}
