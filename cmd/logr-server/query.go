package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net"
	"time"

	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/queryproto"
)

// serveQueryConn handles one client connection on the query socket.
// Protocol: client sends one Request JSON line; server streams Response lines.
func serveQueryConn(ctx context.Context, conn net.Conn, sqlDB *sql.DB) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req queryproto.Request
	if err := dec.Decode(&req); err != nil {
		log.Printf("query: decode request: %v", err)
		return
	}

	// Remove deadline for streaming — long queries or large result sets.
	conn.SetDeadline(time.Time{})

	switch req.Op {
	case queryproto.OpQuery:
		handleQuery(ctx, enc, sqlDB, req)
	case queryproto.OpSources:
		handleSources(ctx, enc, sqlDB)
	case queryproto.OpStats:
		handleStats(ctx, enc, sqlDB, req)
	default:
		enc.Encode(queryproto.Response{Type: queryproto.TypeError, Error: "unknown op: " + req.Op}) //nolint:errcheck
	}
}

func handleQuery(ctx context.Context, enc *json.Encoder, sqlDB *sql.DB, req queryproto.Request) {
	opts := db.QueryOpts{
		Where:    req.Where,
		Args:     req.Args,
		Limit:    req.Limit,
		After:    req.After,
		Before:   req.Before,
		SortCol:  req.SortCol,
		SortDesc: req.SortDesc,
	}
	rows, err := db.Query(ctx, sqlDB, opts)
	if err != nil {
		enc.Encode(queryproto.Response{Type: queryproto.TypeError, Error: err.Error()}) //nolint:errcheck
		return
	}
	for _, r := range rows {
		resp := queryproto.Response{
			Type:    queryproto.TypeRow,
			ID:      r.ID,
			Source:  r.Source,
			Level:   r.Level,
			Message: r.Message,
			Data:    r.Data,
		}
		resp.IngestedAt = r.IngestedAt.UTC().Format(time.RFC3339Nano)
		if r.LogTime != nil {
			resp.LogTime = r.LogTime.UTC().Format(time.RFC3339Nano)
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
	enc.Encode(queryproto.Response{Type: queryproto.TypeDone, Count: len(rows)}) //nolint:errcheck
}

func handleSources(ctx context.Context, enc *json.Encoder, sqlDB *sql.DB) {
	srcs, err := db.Sources(ctx, sqlDB)
	if err != nil {
		enc.Encode(queryproto.Response{Type: queryproto.TypeError, Error: err.Error()}) //nolint:errcheck
		return
	}
	enc.Encode(queryproto.Response{Type: queryproto.TypeSources, Sources: srcs}) //nolint:errcheck
}

func handleStats(ctx context.Context, enc *json.Encoder, sqlDB *sql.DB, req queryproto.Request) {
	opts := db.AggOpts{Where: req.Where, Args: req.Args}

	total, err := db.TotalCount(ctx, sqlDB, opts)
	if err != nil {
		enc.Encode(queryproto.Response{Type: queryproto.TypeError, Error: err.Error()}) //nolint:errcheck
		return
	}
	bySource, err := db.CountBySource(ctx, sqlDB, opts)
	if err != nil {
		enc.Encode(queryproto.Response{Type: queryproto.TypeError, Error: err.Error()}) //nolint:errcheck
		return
	}
	byLevel, err := db.CountByLevel(ctx, sqlDB, opts)
	if err != nil {
		enc.Encode(queryproto.Response{Type: queryproto.TypeError, Error: err.Error()}) //nolint:errcheck
		return
	}
	sourceLevel, err := db.CountBySourceLevel(ctx, sqlDB, opts)
	if err != nil {
		enc.Encode(queryproto.Response{Type: queryproto.TypeError, Error: err.Error()}) //nolint:errcheck
		return
	}

	resp := queryproto.Response{
		Type:        queryproto.TypeStats,
		Total:       total,
		BySource:    toProtoAgg(bySource),
		ByLevel:     toProtoAgg(byLevel),
		SourceLevel: toProtoAgg(sourceLevel),
	}
	enc.Encode(resp) //nolint:errcheck
}

func toProtoAgg(rows []db.AggRow) []queryproto.AggRow {
	out := make([]queryproto.AggRow, len(rows))
	for i, r := range rows {
		out[i] = queryproto.AggRow{Key: r.Key, Key2: r.Key2, Count: r.Count}
	}
	return out
}
