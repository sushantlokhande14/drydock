// Shapes of the API's JSON. They mirror internal/orchestrator and internal/events.

export type RunState =
  | "queued"
  | "provisioning"
  | "running"
  | "verifying"
  | "succeeded"
  | "failed"
  | "cancelled";

export interface Timing {
  queueMs: number;
  sandboxMs: number;
  checkoutMs: number;
  startupMs: number;
  agentMs: number;
  verifyMs: number;
  totalMs: number;
}

export interface Summary {
  id: string;
  title: string;
  repo: string;
  model: string;
  state: RunState;
  phase: string;
  step: number;
  toolCalls: number;
  lastTool?: string;
  warmStart: boolean;
  verified: boolean;
  reason?: string;
  error?: string;
  patch?: { files: number; additions: number; deletions: number };
  timing: Timing;
  createdAt: string;
  finishedAt?: string;
  events: number;
}

export interface Percentiles {
  count: number;
  p50: number;
  p95: number;
  max: number;
  mean: number;
}

export interface Stats {
  active: number;
  peakActive: number;
  queued: number;
  maxConcurrent: number;
  queueSize: number;
  total: number;
  byState: Partial<Record<RunState, number>>;
  pool: { target: number; idle: number; creating: number; warmHits: number; coldMisses: number; createErrors: number };
  startup: { all: Percentiles; warm: Percentiles; cold: Percentiles };
}

export interface TaskInfo {
  name: string;
  title: string;
  issue: string;
}

export interface RunEvent {
  seq: number;
  run: string;
  time: string;
  type: string;
  // The payload depends on the type; see fold() in run.ts for how each is read.
  data: Record<string, unknown>;
}

export const TERMINAL_STATES: RunState[] = ["succeeded", "failed", "cancelled"];

export function isTerminal(state: RunState): boolean {
  return TERMINAL_STATES.includes(state);
}
