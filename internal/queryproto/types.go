// Package queryproto defines the newline-delimited JSON wire types exchanged
// between logr-server's query socket and clients (e.g. logr-view).
//
// Protocol:
//  1. Client connects to the query socket.
//  2. Client writes exactly one Request as a JSON line.
//  3. Server writes one or more Response lines, always ending with
//     a line whose Type is "done" or "error".
//  4. Connection is closed by the client after reading the terminal response.
package queryproto

// Op constants for Request.Op.
const (
	OpQuery   = "query"
	OpSources = "sources"
	OpStats   = "stats"
)

// Type constants for Response.Type.
const (
	TypeRow     = "row"
	TypeDone    = "done"
	TypeError   = "error"
	TypeSources = "sources"
	TypeStats   = "stats"
)

// Request is sent by the client (one JSON line).
type Request struct {
	Op       string `json:"op"`
	Where    string `json:"where,omitempty"`
	Args     []any  `json:"args,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	After    int64  `json:"after,omitempty"`
	Before   int64  `json:"before,omitempty"`
	SortCol  string `json:"sort_col,omitempty"`
	SortDesc bool   `json:"sort_desc,omitempty"`
}

// Response is one JSON line sent by the server.
// The Type field determines which other fields are populated.
type Response struct {
	Type string `json:"type"`

	// TypeRow fields
	ID         int64          `json:"id,omitempty"`
	IngestedAt string         `json:"ingested_at,omitempty"`
	LogTime    string         `json:"log_time,omitempty"`
	Source     string         `json:"source,omitempty"`
	Level      string         `json:"level,omitempty"`
	Message    string         `json:"message,omitempty"`
	Data       map[string]any `json:"data,omitempty"`

	// TypeDone
	Count int `json:"count,omitempty"`

	// TypeError
	Error string `json:"error,omitempty"`

	// TypeSources
	Sources []string `json:"sources,omitempty"`

	// TypeStats
	Total       int64    `json:"total,omitempty"`
	BySource    []AggRow `json:"by_source,omitempty"`
	ByLevel     []AggRow `json:"by_level,omitempty"`
	SourceLevel []AggRow `json:"source_level,omitempty"`
}

// AggRow is one bucket in an aggregation result.
type AggRow struct {
	Key   string `json:"key"`
	Key2  string `json:"key2,omitempty"`
	Count int64  `json:"count"`
}
