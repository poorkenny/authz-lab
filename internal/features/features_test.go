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
	for _, c := range cases {
		env := map[string]string{"FEATURE_CONTROLLED_CACHE_DELIVERY": c.value}
		got := FromEnv(func(k string) string { return env[k] }).ControlledCacheDelivery
		if got != c.want {
			t.Errorf("FEATURE_CONTROLLED_CACHE_DELIVERY=%q: got %v, want %v", c.value, got, c.want)
		}
	}
}
