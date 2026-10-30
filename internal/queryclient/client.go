// Package queryclient connects to logr-server's query socket and executes
// queries without requiring direct access to the SQLite database.
package queryclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/queryproto"
)

// Client sends query requests to logr-server over its query socket.
// Each method opens a fresh connection, sends one request, and reads responses.
// Safe for concurrent use.
type Client struct {
	addr string
}

// StatsResult mirrors the aggregation data the TUI needs.
type StatsResult struct {
	Total       int64
	BySource    []db.AggRow
	ByLevel     []db.AggRow
	SourceLevel []db.AggRow
}

func New(addr string) *Client {
	return &Client{addr: addr}
}

func (c *Client) Query(ctx context.Context, opts db.QueryOpts) ([]db.Row, error) {
	enc, dec, close, err := c.connect()
	if err != nil {
		return nil, err
	}
	defer close()

	req := queryproto.Request{
		Op:       queryproto.OpQuery,
		Where:    opts.Where,
		Args:     opts.Args,
		Limit:    opts.Limit,
		After:    opts.After,
		Before:   opts.Before,
		SortCol:  opts.SortCol,
		SortDesc: opts.SortDesc,
	}
	if err := enc.Encode(req); err != nil {
		return nil, fmt.Errorf("queryclient: send: %w", err)
	}

	var rows []db.Row
	for {
		var resp queryproto.Response
		if err := dec.Decode(&resp); err != nil {
			return nil, fmt.Errorf("queryclient: recv: %w", err)
		}
		switch resp.Type {
		case queryproto.TypeRow:
			rows = append(rows, responseToRow(resp))
		case queryproto.TypeDone:
			return rows, nil
		case queryproto.TypeError:
			return nil, errors.New(resp.Error)
		}
	}
}

func (c *Client) Sources(ctx context.Context) ([]string, error) {
	enc, dec, close, err := c.connect()
	if err != nil {
		return nil, err
	}
	defer close()

	if err := enc.Encode(queryproto.Request{Op: queryproto.OpSources}); err != nil {
		return nil, fmt.Errorf("queryclient: send: %w", err)
	}

	var resp queryproto.Response
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("queryclient: recv: %w", err)
	}
	if resp.Type == queryproto.TypeError {
		return nil, errors.New(resp.Error)
	}
	return resp.Sources, nil
}

func (c *Client) Stats(ctx context.Context, where string, args []any) (*StatsResult, error) {
	enc, dec, close, err := c.connect()
	if err != nil {
		return nil, err
	}
	defer close()

	req := queryproto.Request{Op: queryproto.OpStats, Where: where, Args: args}
	if err := enc.Encode(req); err != nil {
		return nil, fmt.Errorf("queryclient: send: %w", err)
	}

	var resp queryproto.Response
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("queryclient: recv: %w", err)
	}
	if resp.Type == queryproto.TypeError {
		return nil, errors.New(resp.Error)
	}

	return &StatsResult{
		Total:       resp.Total,
		BySource:    fromProtoAgg(resp.BySource),
		ByLevel:     fromProtoAgg(resp.ByLevel),
		SourceLevel: fromProtoAgg(resp.SourceLevel),
	}, nil
}

// connect dials the query socket and returns encoder/decoder/closer.
func (c *Client) connect() (*json.Encoder, *json.Decoder, func(), error) {
	network, addr, err := parseAddr(c.addr)
	if err != nil {
		return nil, nil, nil, err
	}
	conn, err := net.Dial(network, addr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("queryclient: connect %s: %w", c.addr, err)
	}
	return json.NewEncoder(conn), json.NewDecoder(conn), func() { conn.Close() }, nil
}

func parseAddr(addr string) (network, hostaddr string, err error) {
	switch {
	case strings.HasPrefix(addr, "unix://"):
		return "unix", strings.TrimPrefix(addr, "unix://"), nil
	case strings.HasPrefix(addr, "tcp://"):
		return "tcp", strings.TrimPrefix(addr, "tcp://"), nil
	}
	return "", "", fmt.Errorf("queryclient: unknown scheme in %q (want unix:// or tcp://)", addr)
}

func responseToRow(r queryproto.Response) db.Row {
	row := db.Row{
		ID:      r.ID,
		Source:  r.Source,
		Level:   r.Level,
		Message: r.Message,
		Data:    r.Data,
	}
	if r.IngestedAt != "" {
		row.IngestedAt = parseTime(r.IngestedAt)
	}
	if r.LogTime != "" {
		t := parseTime(r.LogTime)
		row.LogTime = &t
	}
	if row.Data == nil {
		row.Data = map[string]any{}
	}
	return row
}

func parseTime(s string) time.Time {
	for _, f := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(f, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func fromProtoAgg(rows []queryproto.AggRow) []db.AggRow {
	out := make([]db.AggRow, len(rows))
	for i, r := range rows {
		out[i] = db.AggRow{Key: r.Key, Key2: r.Key2, Count: r.Count}
	}
	return out
}
