// Package wire defines the newline-delimited JSON protocol between logr-in and logr-server.
package wire

import "time"

// Record is one log event on the wire. The server assigns id and ingested_at on insert.
type Record struct {
	Source  string         `json:"source"`
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message"`
	LogTime *time.Time     `json:"log_time,omitempty"`
	Data    map[string]any `json:"data,omitempty"`
}
