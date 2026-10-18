package sim

// Config is what the reviewer composes in the UI: which sources of staleness
// are active and which defenses are enabled.
type Config struct {
	Sources  Sources  `json:"sources"`
	Defenses Defenses `json:"defenses"`
	Timing   Timing   `json:"timing"`
}

type Sources struct {
	// ReplicationLag delays the changelog to the target replica (replica-2).
	ReplicationLag LagSource `json:"replicationLag"`
	// DecisionCache caches decisions per replica with a TTL.
	DecisionCache CacheSource `json:"decisionCache"`
	// IndirectRevocation revokes through a group membership instead of a direct grant.
	IndirectRevocation bool `json:"indirectRevocation"`
	// Partition cuts replica-2 off from both the changelog and the fast lane
	// just before the revocation.
	Partition bool `json:"partition"`
	// NewEnemy writes new secret content after the revocation, then checks.
	NewEnemy bool `json:"newEnemy"`
}

type LagSource struct {
	Enabled bool  `json:"enabled"`
	LagMs   int64 `json:"lagMs"`
}

type CacheSource struct {
	Enabled bool  `json:"enabled"`
	TTLMs   int64 `json:"ttlMs"`
}

type Defenses struct {
	// AlwaysPrimary sends every check to the primary (fully consistent).
	AlwaysPrimary bool `json:"alwaysPrimary"`
	// Tokens honours the caller's consistency token: answer at >= token revision.
	Tokens bool `json:"tokens"`
	// Watermark: replicas refuse to answer below the latest revocation
	// watermark they have received on the fast lane for the object.
	Watermark bool `json:"watermark"`
	// WatermarkGranularity is "object" (precise, expanded to dependents) or
	// "namespace" (coarse: any revoke in doc:* fences all doc checks).
	WatermarkGranularity string `json:"watermarkGranularity"`
	// RevisionKeyedCache only serves cache entries computed at >= the required revision.
	RevisionKeyedCache bool `json:"revisionKeyedCache"`
	// FailClosed stops serving locally when the fast-lane heartbeat is lost or
	// observed replication lag exceeds LagBoundMs.
	FailClosed bool `json:"failClosed"`
	// FailMode is "forward" (send to primary) or "deny".
	FailMode           string `json:"failMode"`
	WaitBudgetMs       int64  `json:"waitBudgetMs"`
	HeartbeatTimeoutMs int64  `json:"heartbeatTimeoutMs"`
	LagBoundMs         int64  `json:"lagBoundMs"`
}

type Timing struct {
	BaseLagMs          int64   `json:"baseLagMs"`          // lag of healthy replicas
	FastLaneMs         int64   `json:"fastLaneMs"`         // watermark delivery delay
	PrimaryRTTMs       float64 `json:"primaryRttMs"`       // cost of a forwarded / primary check
	LocalMs            float64 `json:"localMs"`            // cost of a local replica check
	CacheMs            float64 `json:"cacheMs"`            // cost of a cache hit
	CheckAfterRevokeMs int64   `json:"checkAfterRevokeMs"` // drill: time between revoke and final check
}

func DefaultConfig() Config {
	return Config{
		Sources: Sources{
			ReplicationLag: LagSource{LagMs: 2000},
			DecisionCache:  CacheSource{TTLMs: 30000},
		},
		Defenses: Defenses{
			WatermarkGranularity: "object",
			FailMode:             "forward",
			WaitBudgetMs:         50,
			HeartbeatTimeoutMs:   1000,
			LagBoundMs:           5000,
		},
		Timing: Timing{
			BaseLagMs:          50,
			FastLaneMs:         20,
			PrimaryRTTMs:       40,
			LocalMs:            0.5,
			CacheMs:            0.1,
			CheckAfterRevokeMs: 200,
		},
	}
}

// Normalize fills defaults and clamps values so a public instance can't be
// asked to simulate absurd configurations.
func (c Config) Normalize() Config {
	d := DefaultConfig()
	clampI := func(v *int64, def, lo, hi int64) {
		if *v == 0 {
			*v = def
		}
		*v = min(max(*v, lo), hi)
	}
	clampF := func(v *float64, def, lo, hi float64) {
		if *v == 0 {
			*v = def
		}
		*v = min(max(*v, lo), hi)
	}
	clampI(&c.Sources.ReplicationLag.LagMs, d.Sources.ReplicationLag.LagMs, 1, 60000)
	clampI(&c.Sources.DecisionCache.TTLMs, d.Sources.DecisionCache.TTLMs, 1, 600000)
	if c.Defenses.WatermarkGranularity != "namespace" {
		c.Defenses.WatermarkGranularity = "object"
	}
	if c.Defenses.FailMode != "deny" {
		c.Defenses.FailMode = "forward"
	}
	if c.Defenses.WaitBudgetMs < 0 {
		c.Defenses.WaitBudgetMs = 0
	}
	c.Defenses.WaitBudgetMs = min(c.Defenses.WaitBudgetMs, 5000)
	clampI(&c.Defenses.HeartbeatTimeoutMs, d.Defenses.HeartbeatTimeoutMs, 10, 60000)
	clampI(&c.Defenses.LagBoundMs, d.Defenses.LagBoundMs, 10, 60000)
	clampI(&c.Timing.BaseLagMs, d.Timing.BaseLagMs, 1, 10000)
	clampI(&c.Timing.FastLaneMs, d.Timing.FastLaneMs, 1, 5000)
	clampF(&c.Timing.PrimaryRTTMs, d.Timing.PrimaryRTTMs, 1, 1000)
	clampF(&c.Timing.LocalMs, d.Timing.LocalMs, 0.01, 100)
	clampF(&c.Timing.CacheMs, d.Timing.CacheMs, 0.01, 100)
	clampI(&c.Timing.CheckAfterRevokeMs, d.Timing.CheckAfterRevokeMs, 1, 60000)
	return c
}

// Presets used by experiments (and mirrored in the UI).
func Preset(name string, base Config) Config {
	c := base
	c.Defenses = Defenses{
		WatermarkGranularity: base.Defenses.WatermarkGranularity,
		FailMode:             "forward",
		WaitBudgetMs:         base.Defenses.WaitBudgetMs,
		HeartbeatTimeoutMs:   base.Defenses.HeartbeatTimeoutMs,
		LagBoundMs:           base.Defenses.LagBoundMs,
	}
	switch name {
	case "full":
		c.Defenses.AlwaysPrimary = true
	case "tokens":
		c.Defenses.Tokens = true
	case "guarded":
		c.Defenses.Tokens = true
		c.Defenses.Watermark = true
		c.Defenses.RevisionKeyedCache = true
		c.Defenses.FailClosed = true
	}
	return c.Normalize()
}

var PresetNames = []string{"naive", "tokens", "guarded", "full"}
