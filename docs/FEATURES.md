# Feature releases

Every feature of the demo ships behind a **feature flag**, so it can be released, rolled back or turned off for a review without a code change.

## Rules for new features

1. Add a flag to `internal/features` as an environment variable named `FEATURE_<NAME>`, with an explicit default.
2. The server logs the active flags at startup and serves them at `GET /api/v1/features`. The UI reads that endpoint and shows or hides the feature accordingly.
3. When a flag is off, the feature's API endpoints return `404` with a message naming the flag.
4. Add the flag to `render.yaml` (`envVars`) with its released value.
5. Document the flag in this file (the table and a section below), in the same pull request as the feature.

## Flags

| Flag | Default | Status | Since |
|---|---|---|---|
| [`FEATURE_CONTROLLED_CACHE_DELIVERY`](#feature_controlled_cache_delivery) | `true` (enabled) | Released | PR #4 |
| [`FEATURE_STRATEGY_STORIES`](#feature_strategy_stories) | `true` (enabled) | Released | PR #7 |
| [`FEATURE_HAPPY_PATH_COMPARISON`](#feature_happy_path_comparison) | `true` (enabled) | Released | PR #7 |
| [`FEATURE_CLIENT_VERSION_TOKEN`](#feature_client_version_token) | `true` (enabled) | Released | PR #9 |
| [`FEATURE_SQLITE_FAILURE`](#feature_sqlite_failure) | `true` (enabled) | Released | PR #10 |
| `FEATURE_AUTH` | `false` | Planned: authentication and user management (see [TODO.md](../TODO.md)) | not implemented |

## How to enable / disable a flag

Flags accept any Go boolean: `true` / `false`, `1` / `0`. An invalid value falls back to the default (a warning is logged). The examples below use `FEATURE_CONTROLLED_CACHE_DELIVERY`; every flag works the same way.

| Where | Enable | Disable |
|---|---|---|
| Local binary | `FEATURE_CONTROLLED_CACHE_DELIVERY=true ./bin/authz-lab` | `FEATURE_CONTROLLED_CACHE_DELIVERY=false ./bin/authz-lab` |
| `go run` | `FEATURE_CONTROLLED_CACHE_DELIVERY=true make dev-api` | `FEATURE_CONTROLLED_CACHE_DELIVERY=false make dev-api` |
| Docker | `docker run -e FEATURE_CONTROLLED_CACHE_DELIVERY=true -p 10000:10000 authz-lab` | `docker run -e FEATURE_CONTROLLED_CACHE_DELIVERY=false -p 10000:10000 authz-lab` |
| Render | Dashboard → service → **Environment** → set `FEATURE_CONTROLLED_CACHE_DELIVERY` = `true` → **Save, rebuild and deploy** | same, with `false` |
| Render (as code) | `value: "true"` in `render.yaml`, then push | `value: "false"` in `render.yaml`, then push |

Check what a running instance uses:

```bash
curl -s https://<your-service>.onrender.com/api/v1/features
# {"controlledCacheDelivery":true,"strategyStories":true,"happyPathComparison":true,"clientVersionToken":true}
```

---

## `FEATURE_CONTROLLED_CACHE_DELIVERY`

**Controlled cache delivery**: invalidation events wait in the delivery queue until a reviewer delivers them from the UI, so the read-after-write inconsistency of the invalidation strategy can be shown step by step, with deterministic results.

- **Default:** `true` (enabled)
- **Code:** `internal/features` (flag), `internal/demo` (`Demo.AutoDeliver`, `Queue.DeliverNext`), `internal/api` (gating), `web/src/App.tsx` (UI)

| | Enabled (`true`) | Disabled (`false`) |
|---|---|---|
| Invalidation events | Wait in the queue until **Deliver next invalidation** is clicked | Delivered automatically right after each SQLite commit |
| Interactive demo story | 5 steps: read → revoke → read before delivery → **deliver** → read after delivery | 3 steps: read → revoke → read again |
| Data-flow diagram | Queue node lists queued events and has a **Deliver next invalidation** button | Queue node shows the event delivered with the write; no button |
| Section 3, queue simulation | Shown | Hidden |
| "Same story, all three strategies" | Columns: read before / after delivery | Columns: read after revoke / read again |
| API | `POST /api/v1/deliver` and `/api/v1/sim/*` available | Those endpoints return `404` |
| `GET /api/v1/features` | `{"controlledCacheDelivery":true}` | `{"controlledCacheDelivery":false}` |

The correctness suite (`go test ./internal/demo` and the UI's **Run correctness tests**) always covers both behaviours, whatever the flag says. `internal/api/api_test.go` checks the gating with the flag on and off.

### Release history

| Date | Change |
|---|---|
| 2026-10-18 | Introduced, enabled by default (PR #4) |

---

## `FEATURE_STRATEGY_STORIES`

**Strategy stories**: each strategy gets its own story in the interactive demo, with only the steps that make sense for it. The TTL cache sends no invalidations, so its story has no manual delivery step. Instead, cache entries expire on a simulated clock, the UI shows the TTL config and each entry's freshness, and a step fast-forwards the clock past the TTL.

- **Default:** `true` (enabled)
- **Code:** `internal/features` (flag), `internal/demo` (`Options.EnforceTTL`, `Demo.Advance`), `internal/api` (`POST /api/v1/clock/advance`, gating), `web/src/App.tsx` (`STORIES`), `web/src/components/CacheFreshness.tsx`

| | Enabled (`true`) | Disabled (`false`) |
|---|---|---|
| TTL story | read → revoke → **read within the TTL** (repeatable) → **fast-forward 300 s** → read after the TTL | the shared story, including a delivery step that has nothing to deliver |
| Invalidation story | read → revoke → **check before delivery** (repeatable, also as Bob) → deliver → read after delivery | the shared story |
| Revision-aware story | read → revoke → read after the revoke (fallback) → read again (cache hit) | the shared story |
| Cache TTL | Entries expire 300 s after they were cached (simulated clock) | Entries never expire |
| Cache TTL and freshness panel | Shown: TTL config, simulated clock, and per entry the revision cached at, SQLite's revision, age, time left, fresh / stale | Hidden |
| **Deliver next invalidation** buttons | Only for the invalidation strategy | For every strategy |
| "Same story, all three strategies" | TTL's last read happens after the TTL: DENY | TTL's last read is still a stale ALLOW |
| API | `POST /api/v1/clock/advance` available | It returns `404` |
| `GET /api/v1/features` | `"strategyStories":true` | `"strategyStories":false` |

### Release history

| Date | Change |
|---|---|
| 2026-10-18 | Introduced, enabled by default (PR #7) |

---

## `FEATURE_HAPPY_PATH_COMPARISON`

**Happy-path comparison**: with the invalidation strategy, a read made while the user's invalidation is still queued also reports the result the read would have had on the happy path (the event delivered before the read). The UI shows the two side by side: the incorrect result actually served and the correct one.

- **Default:** `true` (enabled)
- **Code:** `internal/features` (flag), `internal/demo` (`Options.HappyPath`, `CheckResult.HappyPath`), `web/src/App.tsx` (`HappyPathCompare`)

| | Enabled (`true`) | Disabled (`false`) |
|---|---|---|
| Check response, invalidation queued for that user and document | Includes `happyPath`: decision, served by, latency, hops, note | No `happyPath` field |
| Result card | **This read** (e.g. ✕ ALLOW from the cache, 1.2 ms) next to **Happy path** (✓ DENY from SQLite, 16.3 ms) | Only the read's own result |
| `GET /api/v1/features` | `"happyPathComparison":true` | `"happyPathComparison":false` |

The happy path is computed without changing any state: the queue, the cache and SQLite are untouched. It only applies to the main demo (not the section 3 queue simulation).

### Release history

| Date | Change |
|---|---|
| 2026-10-18 | Introduced, enabled by default (PR #7) |

---

## `FEATURE_CLIENT_VERSION_TOKEN`

**Client version token**: a revision-aware check can carry the client's own version token, which may be older than the document's latest, or ask for a fully consistent read. This shows the design principle of the revision-aware strategy: a check is evaluated at a version **at least as new as the token in the request**, not necessarily the latest. Only a fully consistent read, which skips the cache and asks the single source of truth every time, promises the latest. That is strong consistency without scalability.

- **Default:** `true` (enabled)
- **Code:** `internal/features` (flag), `internal/demo` (`Options.ClientTokens`, `CheckOpts`, `Demo.CheckWith`), `internal/api` (`check` handler), `web/src/App.tsx` (revision-aware story, design-principle note, request-token selector)

| | Enabled (`true`) | Disabled (`false`) |
|---|---|---|
| `POST /api/v1/check` | Accepts `token` (client's version token) and `consistency: "fully_consistent"` for the revision-aware strategy; returns `tokenSource`, `latestToken`, `evaluatedRev`, `contractMet` | Requests with `token` or `consistency` return `404`; checks always use the doc's latest token |
| Revision-aware story (with `FEATURE_STRATEGY_STORIES`) | open doc (client keeps ver#3) → revoke (ver#4) → **check with the old token** (cached ALLOW: promise kept, not the latest) → check with the latest token (DENY) → **fully consistent check** (DENY, always SQLite) | read → revoke → read after the revoke (fallback) → read again (cache hit) |
| Design-principle note | Shown above the revision-aware steps | Hidden |
| Result card | Old-token reads show "✓ promise kept: evaluated at rev N ≥ request token ver#M" and "✕ not the latest" | Not applicable |
| Latency budget | Includes "Fully consistent read (always SQLite): 16.0 ms" | Row hidden |
| Explore freely | Request-token selector: latest, ver#N, or fully consistent | No selector |
| `GET /api/v1/features` | `"clientVersionToken":true` | `"clientVersionToken":false` |

A token newer than the latest committed revision is rejected with `400`, and so is a token or consistency mode sent with the TTL or invalidation strategy. The correctness suite covers the old-token, latest-token and fully consistent cases and runs a fully consistent property test on the shared workloads, whatever the flag says.

### Release history

| Date | Change |
|---|---|
| 2026-10-18 | Introduced, enabled by default (PR #9) |

## `FEATURE_SQLITE_FAILURE`

**SQLite failure simulation**: a separate world (revision-aware strategy, client version tokens always on) where SQLite can be taken down, to show failure handling on a failed read from the primary store. While SQLite is down:

- A check is answered from a **cached snapshot whose revision is equal to or newer than the revision the request asks for** (its version token). The response is a normal `200` with `primaryDown: true`.
- Otherwise the API returns **`503 Service Unavailable`** with `"error": "SQLite is unavailable and no snapshot satisfies the revision: …"`. That covers a snapshot older than the requested version, no cached snapshot at all, and a fully consistent check (which always needs SQLite). It never answers from an older snapshot.
- Writes return `503`: nothing is committed.

The Check API gives up on the unavailable SQLite after a modelled 50 ms timeout, so a 503 costs about 51 ms. The correct-answer oracle still reads SQLite, off the request path.

- **Default:** `true` (enabled)
- **Code:** `internal/features` (flag), `internal/demo` (`Demo.PrimaryDown`, `SetPrimaryDown`, `UnavailableError`), `internal/api` (`/api/v1/outage/*` routes, 503 mapping), `web/src/components/OutageSimulation.tsx` (UI section)

| | Enabled (`true`) | Disabled (`false`) |
|---|---|---|
| `/api/v1/outage/{state,reset,check,grant,revoke}` and `POST /api/v1/outage/primary {down}` | Available. `check` takes `token` and `consistency`; a check that no snapshot satisfies returns `503` with an `unavailable` object (`requiredRev`, `snapshotRev`, `latestToken`, `hops`, `latencyMs`) | `404` |
| UI section "Simulation: SQLite failure" | Shown: failure handling rule, SQLite up/down switch, 7-step story, own-request selector, responses table (200 / 503) | Hidden |
| Main demo and queue simulation | Unaffected: SQLite never fails there | Unaffected |
| `GET /api/v1/features` | `"sqliteFailure":true` | `"sqliteFailure":false` |

The story: Alice opens doc:42 (snapshot at rev 3, her client keeps ver#3) → admin grants Carol (doc:42 → ver#4) → **SQLite fails** → Alice checks with ver#3 (**200** from the snapshot, rev 3 ≥ ver#3) → Alice checks with the latest token (**503**: snapshot rev 3 < ver#4; also try Carol with nothing cached, a fully consistent check, a write) → SQLite recovers → Alice checks with the latest token again (200 from SQLite, snapshot refreshed). The correctness suite covers all three stages whatever the flag says.

### Release history

| Date | Change |
|---|---|
| 2026-10-18 | Introduced, enabled by default (PR #10) |
