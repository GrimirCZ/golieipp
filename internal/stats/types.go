// Package stats records best-effort, action-based usage in a separate database.
package stats

import "time"

// Options controls recording; the zero value is disabled. Retention zero means
// indefinitely. Callers supply defaults rather than interpreting zero here.
type Options struct {
	Enabled         bool
	Path            string
	QueueSize       int
	BatchSize       int
	FlushInterval   time.Duration
	DetailRetention time.Duration
	CPURetention    time.Duration
	RollupRetention time.Duration
	Resources       bool
	CPU             bool
}

// Action is one completed operation. Nested actions have separate kinds and
// parent IDs: durations and bytes must not be summed across hierarchy levels.
type Action struct {
	ID               string           `json:"id"`
	ParentID         string           `json:"parent_id,omitempty"`
	RunID            string           `json:"run_id"`
	Kind             string           `json:"kind"`
	Operation        string           `json:"operation"`
	Queue            string           `json:"queue,omitempty"`
	User             string           `json:"user,omitempty"`
	Owner            string           `json:"owner,omitempty"`
	RegistryID       string           `json:"registry_id,omitempty"`
	JobID            int              `json:"job_id,omitempty"`
	Outcome          string           `json:"outcome"`
	Reason           string           `json:"reason,omitempty"`
	HTTPStatus       int              `json:"http_status,omitempty"`
	IPPStatus        int              `json:"ipp_status,omitempty"`
	StartedAt        time.Time        `json:"started_at"`
	FinishedAt       time.Time        `json:"finished_at"`
	DurationNS       int64            `json:"duration_ns"`
	ClientBytes      int64            `json:"client_bytes"`
	UpstreamBytes    int64            `json:"upstream_bytes"`
	OutputBytes      int64            `json:"output_bytes"`
	BufferPeakBytes  int64            `json:"buffer_peak_bytes"`
	BufferComponents map[string]int64 `json:"buffer_components,omitempty"`
	TempPeakBytes    int64            `json:"temp_peak_bytes"`
	TempWrittenBytes int64            `json:"temp_written_bytes"`
	Concurrency      int64            `json:"concurrency"`
	UserConcurrency  int64            `json:"user_concurrency"`
}

// Job is a complete accounting snapshot. Unknown numeric facts are nil.
// The registry UUID scopes IDs even when the primary database is replaced.
type Job struct {
	// Register marks the first snapshot for a newly reserved proxy job. A
	// snapshot for an unknown key is ignored unless Register is true; this is
	// what keeps startup reconciliation from backfilling historical jobs.
	Register             bool       `json:"register,omitempty"`
	RegistryID           string     `json:"registry_id"`
	JobID                int        `json:"job_id"`
	Queue                string     `json:"queue"`
	User                 string     `json:"user"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	ProcessingAt         *time.Time `json:"processing_at,omitempty"`
	TerminalAt           *time.Time `json:"terminal_at,omitempty"`
	State                string     `json:"state"`
	ObservedState        string     `json:"observed_state,omitempty"`
	StateReasons         string     `json:"state_reasons,omitempty"`
	Accepted             bool       `json:"accepted"`
	CancelAccepted       bool       `json:"cancel_accepted"`
	DocumentCount        int        `json:"document_count"`
	Copies               int        `json:"copies"`
	PayloadBytes         int64      `json:"payload_bytes"`
	PageCount            *int       `json:"page_count,omitempty"`
	EstimatedImpressions *int       `json:"estimated_impressions,omitempty"`
	CompletedImpressions *int       `json:"completed_impressions,omitempty"`
	CompletedSheets      *int       `json:"completed_sheets,omitempty"`
	DocumentFormat       string     `json:"document_format,omitempty"`
	UpstreamFormat       string     `json:"upstream_format,omitempty"`
	Route                string     `json:"route,omitempty"`
	RequestedColor       string     `json:"requested_color,omitempty"`
	EffectiveColor       string     `json:"effective_color,omitempty"`
	RequestedSides       string     `json:"requested_sides,omitempty"`
	EffectiveSides       string     `json:"effective_sides,omitempty"`
	RequestedMedia       string     `json:"requested_media,omitempty"`
	EffectiveMedia       string     `json:"effective_media,omitempty"`
}

type CPUReading struct {
	Available     bool      `json:"available"`
	UserSeconds   float64   `json:"user_seconds"`
	SystemSeconds float64   `json:"system_seconds"`
	At            time.Time `json:"at"`
}

type Health struct {
	Enabled       bool      `json:"enabled"`
	Dropped       uint64    `json:"dropped"`
	Errors        uint64    `json:"errors"`
	LastPersisted time.Time `json:"last_persisted"`
	LastError     string    `json:"last_error,omitempty"`
}

type ReportOptions struct {
	Command string
	Since   time.Time
	Until   time.Time
	Queue   string
	User    string
	UserSet bool
	Group   string
	Limit   int
	Offset  int
}

type Report struct {
	Columns  []string         `json:"columns"`
	Rows     []map[string]any `json:"rows"`
	Metadata map[string]any   `json:"metadata"`
}
