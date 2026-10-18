// Package features holds the feature flags that gate the demo's features so
// each one can be released or rolled back independently. Every flag is
// documented in docs/FEATURES.md.
package features

import (
	"log"
	"strconv"
)

// Flags is the set of feature flags, also served to the UI at GET /api/v1/features.
type Flags struct {
	// ControlledCacheDelivery: invalidation events wait in the delivery queue
	// until a reviewer delivers them from the UI (deterministic demo). When
	// off, each event is delivered automatically right after its commit.
	ControlledCacheDelivery bool `json:"controlledCacheDelivery"`
	// StrategyStories: each strategy gets its own story in the interactive
	// demo. TTL has no delivery step; instead cache entries expire on a
	// simulated clock and the UI shows the TTL config and each entry's
	// freshness, with a fast-forward step.
	StrategyStories bool `json:"strategyStories"`
	// HappyPathComparison: an invalidation-strategy read made while the
	// user's invalidation is still queued also reports the happy-path result
	// (event delivered first), so the UI can show both side by side.
	HappyPathComparison bool `json:"happyPathComparison"`
}

// Defaults are the flag values used when no environment variable is set.
func Defaults() Flags {
	return Flags{ControlledCacheDelivery: true, StrategyStories: true, HappyPathComparison: true}
}

// FromEnv reads flags from environment variables, falling back to Defaults
// for unset or invalid values.
func FromEnv(getenv func(string) string) Flags {
	f := Defaults()
	f.ControlledCacheDelivery = boolFlag(getenv, "FEATURE_CONTROLLED_CACHE_DELIVERY", f.ControlledCacheDelivery)
	f.StrategyStories = boolFlag(getenv, "FEATURE_STRATEGY_STORIES", f.StrategyStories)
	f.HappyPathComparison = boolFlag(getenv, "FEATURE_HAPPY_PATH_COMPARISON", f.HappyPathComparison)
	return f
}

func boolFlag(getenv func(string) string, name string, def bool) bool {
	v := getenv(name)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("feature flag %s=%q is not a boolean; using default %v", name, v, def)
		return def
	}
	return b
}
