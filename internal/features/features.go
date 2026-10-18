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
}

// Defaults are the flag values used when no environment variable is set.
func Defaults() Flags {
	return Flags{ControlledCacheDelivery: true}
}

// FromEnv reads flags from environment variables, falling back to Defaults
// for unset or invalid values.
func FromEnv(getenv func(string) string) Flags {
	f := Defaults()
	f.ControlledCacheDelivery = boolFlag(getenv, "FEATURE_CONTROLLED_CACHE_DELIVERY", f.ControlledCacheDelivery)
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
