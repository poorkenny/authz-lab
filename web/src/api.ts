import type { CheckOpts, CheckResult, ClockResult, CompareRow, DeliverResult, Features, PrimaryResult, ReplayResult, State, Strategy, SuiteResult, WriteResult } from "./types";

const SESSION_KEY = "authz-lab-session";
let memorySession = "";

// One isolated demo session per browser, so reviewers never share state.
function sessionId(): string {
  const fresh = () => crypto.randomUUID().replace(/-/g, "");
  try {
    let id = localStorage.getItem(SESSION_KEY);
    if (!id) {
      id = fresh();
      localStorage.setItem(SESSION_KEY, id);
    }
    return id;
  } catch {
    memorySession ||= fresh();
    return memorySession;
  }
}

// ApiError carries the HTTP status so callers can tell "busy" (409) or
// "unavailable" (503) from failures, and the response body for details.
export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
    readonly body: Record<string, unknown> = {},
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: { "Content-Type": "application/json", "X-Lab-Session": sessionId() },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(data.error ?? `${res.status} ${res.statusText}`, res.status, data);
  return data as T;
}

// The same operations exist on the main demo (/api/v1) and the queue simulation (/api/v1/sim).
function world(prefix: string) {
  return {
    state: () => call<State>("GET", `${prefix}/state`),
    reset: () => call<State>("POST", `${prefix}/reset`),
    check: (subject: string, object: string, opts?: CheckOpts) => call<CheckResult>("POST", `${prefix}/check`, { subject, object, ...opts }),
    grant: (subject: string, object: string) => call<WriteResult>("POST", `${prefix}/grant`, { subject, object }),
    revoke: (subject: string, object: string) => call<WriteResult>("POST", `${prefix}/revoke`, { subject, object }),
    deliver: () => call<DeliverResult>("POST", `${prefix}/deliver`),
  };
}

export const api = {
  ...world("/api/v1"),
  features: () => call<Features>("GET", "/api/v1/features"),
  setStrategy: (strategy: Strategy) => call<State>("POST", "/api/v1/strategy", { strategy }),
  advanceClock: (seconds: number) => call<ClockResult>("POST", "/api/v1/clock/advance", { seconds }),
  compare: () => call<{ rows: CompareRow[] }>("GET", "/api/v1/compare"),
  runTests: () => call<SuiteResult>("POST", "/api/v1/tests/run"),
  sampleLog: () => call<{ log: string; invalidationMs: number }>("GET", "/api/v1/tests/sample-log"),
  replay: (log: string, invalidationMs: number) => call<ReplayResult>("POST", "/api/v1/tests/replay", { log, invalidationMs }),
  sim: {
    ...world("/api/v1/sim"),
    otherWrites: (count: number) => call<WriteResult[]>("POST", "/api/v1/sim/other-writes", { count }),
  },
  // The SQLite-failure simulation (FEATURE_SQLITE_FAILURE): no delivery queue.
  outage: {
    state: () => call<State>("GET", "/api/v1/outage/state"),
    reset: () => call<State>("POST", "/api/v1/outage/reset"),
    check: (subject: string, object: string, opts?: CheckOpts) => call<CheckResult>("POST", "/api/v1/outage/check", { subject, object, ...opts }),
    grant: (subject: string, object: string) => call<WriteResult>("POST", "/api/v1/outage/grant", { subject, object }),
    revoke: (subject: string, object: string) => call<WriteResult>("POST", "/api/v1/outage/revoke", { subject, object }),
    setPrimary: (down: boolean) => call<PrimaryResult>("POST", "/api/v1/outage/primary", { down }),
  },
};
