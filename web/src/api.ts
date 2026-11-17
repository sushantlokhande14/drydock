import { useEffect, useRef, useState } from "react";
import { emptyRun, fold, type RunView } from "./run";
import type { RunEvent, Stats, Summary, TaskInfo } from "./types";

async function json<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, init);
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.error ?? `${response.status} ${response.statusText}`);
  }
  return response.status === 204 ? (undefined as T) : response.json();
}

export const api = {
  tasks: () => json<TaskInfo[]>("/api/tasks"),
  stats: () => json<Stats>("/api/stats"),
  launch: (task: string, model: string, count: number) =>
    json<Summary[]>(`/api/tasks/${encodeURIComponent(task)}/runs`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ model, count }),
    }),
  cancel: (id: string) => json<void>(`/api/runs/${id}/cancel`, { method: "POST" }),
  patch: (id: string) => json<{ run: Summary; patch: string }>(`/api/runs/${id}`),
};

export type Connection = "connecting" | "live" | "reconnecting";

/**
 * Every run's summary, kept current over one server-sent-event stream: a
 * snapshot on connect, then batched changes. EventSource reconnects on its
 * own; each reconnect starts from a fresh snapshot, so nothing drifts.
 */
export function useFleet(): { runs: Summary[]; stats: Stats | null; connection: Connection } {
  const [runs, setRuns] = useState<Map<string, Summary>>(new Map());
  const [stats, setStats] = useState<Stats | null>(null);
  const [connection, setConnection] = useState<Connection>("connecting");

  useEffect(() => {
    const source = new EventSource("/api/fleet");
    source.addEventListener("open", () => setConnection("live"));
    source.addEventListener("error", () => setConnection("reconnecting"));
    source.addEventListener("snapshot", (event) => {
      const list = JSON.parse((event as MessageEvent).data) as Summary[];
      setRuns(new Map(list.map((run) => [run.id, run])));
      api.stats().then(setStats).catch(() => undefined);
    });
    source.addEventListener("runs", (event) => {
      const batch = JSON.parse((event as MessageEvent).data) as { runs: Summary[]; stats: Stats };
      setStats(batch.stats);
      setRuns((current) => {
        const next = new Map(current);
        for (const run of batch.runs) next.set(run.id, run);
        return next;
      });
    });
    return () => source.close();
  }, []);

  // Newest first. Ids start with a millisecond timestamp, so sorting by id is sorting by creation time.
  const sorted = [...runs.values()].sort((a, b) => (a.id < b.id ? 1 : -1));
  return { runs: sorted, stats, connection };
}

/**
 * One run, rebuilt from its event log. The browser resumes a dropped stream
 * from the last event id it received, so the fold never sees a gap; replayed
 * events are ignored by sequence number.
 */
export function useRun(id: string): { run: RunView; connection: Connection; missing: boolean } {
  const [run, setRun] = useState<RunView>(emptyRun);
  const [connection, setConnection] = useState<Connection>("connecting");
  const [missing, setMissing] = useState(false);
  const pending = useRef<RunEvent[]>([]);

  useEffect(() => {
    setRun(emptyRun());
    setMissing(false);
    pending.current = [];
    let frame = 0;
    let opened = false;

    // Terminal output can arrive as hundreds of events a second. Fold them once per frame.
    const flush = () => {
      frame = 0;
      const batch = pending.current;
      pending.current = [];
      if (batch.length) setRun((current) => batch.reduce(fold, current));
    };

    const source = new EventSource(`/api/runs/${id}/events`);
    source.addEventListener("open", () => {
      opened = true;
      setConnection("live");
    });
    source.addEventListener("message", (event) => {
      pending.current.push(JSON.parse(event.data) as RunEvent);
      if (!frame) frame = requestAnimationFrame(flush);
    });
    source.addEventListener("end", () => {
      // The run is over: stop the browser from reconnecting to a finished stream.
      source.close();
      setConnection("live");
    });
    source.addEventListener("error", () => {
      if (!opened) {
        source.close();
        setMissing(true);
      } else {
        setConnection("reconnecting");
      }
    });
    return () => {
      source.close();
      if (frame) cancelAnimationFrame(frame);
    };
  }, [id]);

  return { run, connection, missing };
}
