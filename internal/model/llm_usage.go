package model

// LLMUsageEvent is one metered /chat upstream call (migration 022). Status
// mirrors the handler's relay result: "ok" | "disconnected" |
// "upstream_error" — the row exists whenever the upstream returned 200, so
// partially-consumed tokens are still recorded.
type LLMUsageEvent struct {
	UserID        string
	AppID         string
	Model         string // logical model id the client picked
	Provider      string
	UpstreamModel string
	Status        string
	InputTokens   int
	OutputTokens  int
	CostMicros    int64 // µ¥ = 1e-6 CNY, = tokens × price_per_mtok (see llm.Model)
}

// LLMUsageRow is the per-model aggregate returned by the admin stats
// endpoint (GET /admin/stats/llm-usage).
type LLMUsageRow struct {
	Model        string `db:"model" json:"model"`
	Requests     int    `db:"requests" json:"requests"`
	InputTokens  int64  `db:"input_tokens" json:"input_tokens"`
	OutputTokens int64  `db:"output_tokens" json:"output_tokens"`
	CostMicros   int64  `db:"cost_micros" json:"cost_micros"`
}
