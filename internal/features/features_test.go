package features

import "testing"

func TestFromEnv(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", true}, // default: enabled
		{"true", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"maybe", true}, // invalid falls back to the default
	}
	flags := map[string]func(Flags) bool{
		"FEATURE_CONTROLLED_CACHE_DELIVERY": func(f Flags) bool { return f.ControlledCacheDelivery },
		"FEATURE_STRATEGY_STORIES":          func(f Flags) bool { return f.StrategyStories },
		"FEATURE_HAPPY_PATH_COMPARISON":     func(f Flags) bool { return f.HappyPathComparison },
		"FEATURE_CLIENT_VERSION_TOKEN":      func(f Flags) bool { return f.ClientVersionToken },
		"FEATURE_SQLITE_FAILURE":            func(f Flags) bool { return f.SQLiteFailure },
		"FEATURE_LOG_REPLAY":                func(f Flags) bool { return f.LogReplay },
		"FEATURE_MULTI_POOL_INVALIDATION":   func(f Flags) bool { return f.MultiPoolInvalidation },
	}
	for name, get := range flags {
		for _, c := range cases {
			env := map[string]string{name: c.value}
			if got := get(FromEnv(func(k string) string { return env[k] })); got != c.want {
				t.Errorf("%s=%q: got %v, want %v", name, c.value, got, c.want)
			}
		}
	}
}
