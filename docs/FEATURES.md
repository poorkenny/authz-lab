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
# {"controlledCacheDelivery":true,"strategyStories":true,"happyPathComparison":true}
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
