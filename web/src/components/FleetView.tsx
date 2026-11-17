import { useEffect, useMemo, useState } from "react";
import { api, useFleet } from "../api";
import { stateLabel } from "../run";
import { isTerminal, type RunState, type Stats, type Summary, type TaskInfo } from "../types";

const PHASES = ["provisioning", "analysis", "editing", "testing", "delivering", "verifying"];

type Filter = "all" | "active" | "succeeded" | "failed";

export function FleetView() {
  const { runs, stats, connection } = useFleet();
  const [filter, setFilter] = useState<Filter>("all");

  const counts = useMemo(() => {
    const active = runs.filter((run) => !isTerminal(run.state)).length;
    const succeeded = runs.filter((run) => run.state === "succeeded").length;
    return { all: runs.length, active, succeeded, failed: runs.length - active - succeeded };
  }, [runs]);

  const shown = runs.filter((run) => {
    if (filter === "active") return !isTerminal(run.state);
    if (filter === "succeeded") return run.state === "succeeded";
    if (filter === "failed") return run.state === "failed" || run.state === "cancelled";
    return true;
  });

  return (
    <main className="fleet">
      <StatsBar stats={stats} connection={connection} />
      <Launcher />
      <div className="filters" role="tablist" aria-label="Filter runs">
        {(["all", "active", "succeeded", "failed"] as Filter[]).map((name) => (
          <button
            key={name}
            role="tab"
            aria-selected={filter === name}
            className={filter === name ? "chip selected" : "chip"}
            onClick={() => setFilter(name)}
          >
            {name} <span className="count">{counts[name]}</span>
          </button>
        ))}
      </div>
      {shown.length === 0 ? (
        <p className="empty">{runs.length === 0 ? "No runs yet. Launch one above." : "No runs match this filter."}</p>
      ) : (
        <div className="grid">
          {shown.map((run) => (
            <Tile key={run.id} run={run} />
          ))}
        </div>
      )}
    </main>
  );
}

function StatsBar({ stats, connection }: { stats: Stats | null; connection: string }) {
  if (!stats) return <section className="stats skeleton" aria-busy="true" />;
  const startup = stats.startup;
  return (
    <section className="stats" aria-label="System">
      <Stat label="Running" value={`${stats.active}`} hint={`of ${stats.maxConcurrent} at once, peak ${stats.peakActive}`} />
      <Stat label="Queued" value={`${stats.queued}`} hint={`room for ${stats.queueSize}`} />
      <Stat label="Warm sandboxes" value={`${stats.pool.idle}`} hint={`pool of ${stats.pool.target}, ${stats.pool.creating} starting`} />
      <Stat
        label="Startup p50"
        value={startup.all.count ? `${startup.all.p50} ms` : "n/a"}
        hint={
          startup.all.count
            ? `warm ${startup.warm.count ? startup.warm.p50 + " ms" : "n/a"}, cold ${startup.cold.count ? startup.cold.p50 + " ms" : "n/a"}, p95 ${startup.all.p95} ms`
            : "no runs yet"
        }
      />
      <Stat label="Verified" value={`${stats.byState.succeeded ?? 0}`} hint={`of ${stats.total} runs`} />
      <span className={`connection ${connection}`} title={`event stream: ${connection}`}>
        {connection === "live" ? "live" : connection}
      </span>
    </section>
  );
}

function Stat({ label, value, hint }: { label: string; value: string; hint: string }) {
  return (
    <div className="stat">
      <span className="stat-label">{label}</span>
      <span className="stat-value">{value}</span>
      <span className="stat-hint">{hint}</span>
    </div>
  );
}

function Launcher() {
  const [tasks, setTasks] = useState<TaskInfo[]>([]);
  const [task, setTask] = useState("");
  const [model, setModel] = useState("scripted");
  const [count, setCount] = useState(1);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  useEffect(() => {
    api
      .tasks()
      .then((list) => {
        setTasks(list);
        if (list.length) setTask(list[0].name);
      })
      .catch((error: Error) => setMessage(error.message));
  }, []);

  const launch = async (names: string[], each: number) => {
    setBusy(true);
    setMessage(null);
    try {
      let started = 0;
      for (const name of names) started += (await api.launch(name, model, each)).length;
      setMessage(`Started ${started} run${started === 1 ? "" : "s"}.`);
    } catch (error) {
      setMessage((error as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const selected = tasks.find((candidate) => candidate.name === task);
  return (
    <section className="launcher" aria-label="Launch runs">
      <div className="launcher-row">
        <label>
          Task
          <select value={task} onChange={(event) => setTask(event.target.value)}>
            {tasks.map((candidate) => (
              <option key={candidate.name} value={candidate.name}>
                {candidate.title}
              </option>
            ))}
          </select>
        </label>
        <label>
          Model
          <select value={model} onChange={(event) => setModel(event.target.value)}>
            <option value="scripted">scripted (replays a recorded solution)</option>
            <option value="ollama:qwen2.5:7b">qwen2.5:7b via Ollama</option>
          </select>
        </label>
        <label className="narrow">
          Runs
          <input
            type="number"
            min={1}
            max={512}
            value={count}
            onChange={(event) => setCount(Math.max(1, Math.min(512, Number(event.target.value) || 1)))}
          />
        </label>
        <button className="primary" disabled={busy || !task} onClick={() => launch([task], count)}>
          Launch
        </button>
        <button
          disabled={busy || tasks.length === 0}
          title="Spread across every task"
          onClick={() => launch(tasks.map((candidate) => candidate.name), Math.ceil(count / Math.max(tasks.length, 1)))}
        >
          Spread across all tasks
        </button>
      </div>
      {selected && <p className="issue">{selected.issue}</p>}
      {message && (
        <p className="launcher-message" role="status">
          {message}
        </p>
      )}
    </section>
  );
}

function progress(run: Summary): number {
  if (run.state === "queued") return 0;
  if (isTerminal(run.state)) return PHASES.length;
  const index = PHASES.indexOf(run.phase);
  return index < 0 ? 1 : index + 1;
}

function Tile({ run }: { run: Summary }) {
  const done = progress(run);
  return (
    <a className={`tile state-${run.state}`} href={`#/runs/${run.id}`}>
      <span className="tile-head">
        <span className="tile-state">{tileLabel(run.state, run.phase)}</span>
        <span className="tile-id">{run.id.slice(-6)}</span>
      </span>
      <span className="tile-title">{run.title}</span>
      <span className="tile-meta">
        {run.state === "queued" ? "waiting for a worker" : run.lastTool ? `step ${run.step} · ${run.lastTool}` : run.model}
      </span>
      <span className="tile-progress" aria-hidden="true">
        {PHASES.map((phase, index) => (
          <i key={phase} className={index < done ? "on" : ""} />
        ))}
      </span>
    </a>
  );
}

function tileLabel(state: RunState, phase: string): string {
  if (state === "running" && phase) return phase;
  return stateLabel(state).toLowerCase();
}
