import { useEffect, useMemo, useRef, useState } from "react";
import { api, useRun } from "../api";
import { describeCall, reasonLabel, stateLabel, type RunView, type StepView, type ToolCallView } from "../run";
import { isTerminal } from "../types";
import { Diff } from "./Diff";

type Tab = "terminal" | "changes" | "verification" | "issue";

export function RunDetail({ id }: { id: string }) {
  const { run, connection, missing } = useRun(id);
  const [tab, setTab] = useState<Tab>("terminal");
  const [userChoseTab, setUserChoseTab] = useState(false);

  // Follow the run: when the patch arrives show it, when the verdict arrives show that.
  // Stop following as soon as the user picks a tab themselves.
  useEffect(() => {
    if (userChoseTab) return;
    if (run.verify) setTab("verification");
    else if (run.diff) setTab("changes");
  }, [run.diff, run.verify, userChoseTab]);

  if (missing) {
    return (
      <main className="run">
        <a className="back" href="#/">
          Back to the fleet
        </a>
        <p className="empty">There is no run with this id.</p>
      </main>
    );
  }

  const choose = (next: Tab) => {
    setUserChoseTab(true);
    setTab(next);
  };

  return (
    <main className="run">
      <a className="back" href="#/">
        Back to the fleet
      </a>
      <header className="run-head">
        <div>
          <h1>{run.title || "Loading"}</h1>
          <p className="run-meta">
            <span className={`pill state-${run.state}`}>{stateLabel(run.state)}</span>
            {run.model && <span>{run.model}</span>}
            {run.sandbox && (
              <span>
                sandbox {run.sandbox} ({run.warm ? "warm" : "cold"} start)
              </span>
            )}
            {run.timing && run.timing.startupMs > 0 && <span>startup {run.timing.startupMs} ms</span>}
            {run.timing && run.timing.totalMs > 0 && <span>total {(run.timing.totalMs / 1000).toFixed(1)} s</span>}
            {connection === "reconnecting" && <span className="warn">reconnecting</span>}
          </p>
          {isTerminal(run.state) && run.reason && (
            <p className={`verdict ${run.state}`}>
              {stateLabel(run.state)}: {reasonLabel(run.reason)}.{run.error ? ` ${run.error}` : ""}
            </p>
          )}
        </div>
        {!isTerminal(run.state) && (
          <button className="danger" onClick={() => api.cancel(id).catch(() => undefined)}>
            Cancel run
          </button>
        )}
      </header>

      <div className="run-body">
        <section className="timeline" aria-label="What the agent did">
          <Timeline run={run} />
        </section>
        <section className="inspector">
          <div className="tabs" role="tablist">
            <TabButton name="terminal" current={tab} onSelect={choose} label="Terminal" />
            <TabButton
              name="changes"
              current={tab}
              onSelect={choose}
              label={run.patchStats ? `Changes +${run.patchStats.additions} -${run.patchStats.deletions}` : "Changes"}
            />
            <TabButton name="verification" current={tab} onSelect={choose} label="Verification" />
            <TabButton name="issue" current={tab} onSelect={choose} label="Issue" />
          </div>
          <div className="tab-body" role="tabpanel">
            {tab === "terminal" && <Terminal run={run} />}
            {tab === "changes" &&
              (run.diff ? <Diff diff={run.diff} /> : <p className="empty">The agent has not produced a patch yet.</p>)}
            {tab === "verification" && <Verification run={run} />}
            {tab === "issue" && (
              <div className="issue-text">
                <p>{run.issue}</p>
                <p className="muted">Repository: {run.repo}</p>
              </div>
            )}
          </div>
        </section>
      </div>
    </main>
  );
}

function TabButton({ name, current, onSelect, label }: { name: Tab; current: Tab; onSelect: (tab: Tab) => void; label: string }) {
  return (
    <button role="tab" aria-selected={current === name} className={current === name ? "tab selected" : "tab"} onClick={() => onSelect(name)}>
      {label}
    </button>
  );
}

function Timeline({ run }: { run: RunView }) {
  if (run.timeline.length === 0) return <p className="empty">Waiting for a worker.</p>;
  return (
    <ol>
      {run.timeline.map((entry, index) =>
        "milestone" in entry ? (
          <li key={index} className={`milestone ${entry.milestone.tone ?? ""}`}>
            <span className="dot" aria-hidden="true" />
            <div>
              <span className="milestone-text">{entry.milestone.text}</span>
              {entry.milestone.detail && <span className="milestone-detail">{entry.milestone.detail}</span>}
            </div>
          </li>
        ) : (
          <Step key={index} step={run.steps[entry.stepIndex]} />
        ),
      )}
    </ol>
  );
}

function Step({ step }: { step: StepView }) {
  return (
    <li className="step">
      <span className="dot" aria-hidden="true" />
      <div className="step-body">
        <div className="step-head">
          <span>Step {step.step}</span>
          {step.modelMs !== undefined && <span className="muted">model answered in {formatMs(step.modelMs)}</span>}
        </div>
        {step.reasoning && <p className="reasoning">{step.reasoning}</p>}
        {step.calls.map((call) => (
          <Call key={call.id} call={call} />
        ))}
      </div>
    </li>
  );
}

function Call({ call }: { call: ToolCallView }) {
  const running = call.result === undefined;
  const status = running ? "running" : call.isError ? "error" : "ok";
  return (
    <details className={`call ${status}`}>
      <summary>
        <span className="call-name">{call.name}</span>
        <span className="call-desc">{describeCall(call)}</span>
        <span className="call-status">
          {running ? "running" : call.isError ? (call.exitCode !== undefined ? `exit ${call.exitCode}` : "error") : "ok"}
          {call.durationMs !== undefined && call.durationMs >= 100 ? ` · ${formatMs(call.durationMs)}` : ""}
        </span>
      </summary>
      {call.name === "replace_in_file" && typeof call.args.old_text === "string" && (
        <pre className="snippet">
          <span className="del">{prefixLines(call.args.old_text, "- ")}</span>
          {"\n"}
          <span className="add">{prefixLines(String(call.args.new_text ?? ""), "+ ")}</span>
        </pre>
      )}
      <pre className="result">{call.result ?? call.output ?? ""}</pre>
    </details>
  );
}

function prefixLines(text: string, prefix: string): string {
  return text
    .split("\n")
    .map((line) => prefix + line)
    .join("\n");
}

function formatMs(ms: number): string {
  return ms >= 1000 ? `${(ms / 1000).toFixed(1)} s` : `${ms} ms`;
}

/** Everything the agent ran in a shell, in order, the way a terminal would show it. */
function Terminal({ run }: { run: RunView }) {
  const lines = useMemo(() => {
    const out: Array<{ kind: "prompt" | "output" | "status"; text: string }> = [];
    for (const step of run.steps) {
      for (const call of step.calls) {
        if (call.name !== "run_command" && call.name !== "run_tests") continue;
        out.push({ kind: "prompt", text: call.name === "run_tests" ? "run_tests" : String(call.args.command ?? "") });
        // While the command runs only the stream exists; afterwards the stream is still the fuller record.
        const body = call.output || call.result || "";
        if (body) out.push({ kind: "output", text: body.replace(/\n$/, "") });
        if (call.result !== undefined && call.exitCode !== undefined && call.exitCode !== 0) {
          out.push({ kind: "status", text: `exit ${call.exitCode}` });
        }
      }
    }
    return out;
  }, [run.steps]);

  const box = useRef<HTMLDivElement>(null);
  const pinned = useRef(true);
  useEffect(() => {
    // Stay at the bottom while output arrives, unless the user has scrolled up to read.
    if (box.current && pinned.current) box.current.scrollTop = box.current.scrollHeight;
  }, [lines]);

  if (lines.length === 0) return <p className="empty">The agent has not run a command yet.</p>;
  return (
    <div
      className="terminal"
      ref={box}
      onScroll={(event) => {
        const el = event.currentTarget;
        pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
      }}
    >
      {lines.map((line, index) => (
        <pre key={index} className={`term-${line.kind}`}>
          {line.kind === "prompt" ? "$ " : ""}
          {line.text}
        </pre>
      ))}
    </div>
  );
}

function Verification({ run }: { run: RunView }) {
  if (!run.verify) {
    return (
      <p className="empty">
        {run.state === "verifying"
          ? "Applying the patch to a clean checkout in a second sandbox and running the hidden tests."
          : "A patch is verified in a second sandbox, on a clean checkout, against tests the agent never saw."}
      </p>
    );
  }
  return (
    <div className="verification">
      <p className={run.verify.passed ? "verdict succeeded" : "verdict failed"}>
        {run.verify.passed ? "Passed" : "Failed"} on a clean checkout in {formatMs(run.verify.ms)}.
      </p>
      <pre className="result open">{run.verify.output}</pre>
    </div>
  );
}
