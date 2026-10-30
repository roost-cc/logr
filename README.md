# logr

Local log aggregation and viewer for development stacks — a Sumo Logic / Datadog-style tool for development. Collects logs from Docker Compose containers, Chrome/Node DevTools targets, or arbitrary stdin sources, stores them in SQLite, and presents them in a fast terminal UI.

```
┌─ logr ────────────────────────────────────────── ● TAIL  [?] help ──┐
│ filter: source=storage AND level!=info                              │
├────────────────┬──────────┬───────┬─────────────────────────────────┤
│ TIME           │ SOURCE   │ LEVEL │ MESSAGE                         │
├────────────────┼──────────┼───────┼─────────────────────────────────┤
│ 23:21:08.826   │ synapse  │ INFO  │ Processed request: GET /_matr…  │
│ 23:21:09.003   │ storage  │ INFO  │ <- [200] /health              * │ ← marked
│ 23:21:09.104   │ account  │ WARN  │ <- [403] /auth                  │
│ ── detail ──────────────────────────────────────────────────────────┤
│ { "source": "storage", "level": "INFO",                             │
│   "message": "<- [200] /health", "_raw": "storage-1  | ..." }       │
├─────────────────────────────────────────────────────────────────────┤
│ rows:1,247  marked:1  [f]ilter [t]ail [m]ark [e]xport               │
└─────────────────────────────────────────────────────────────────────┘
```

## Architecture

Three binaries share one Go module. They communicate over newline-delimited JSON on either unix or TCP sockets — `logr-view` never touches the database directly.

```
  log sources ──▶  logr-in  ──(ingest socket)──▶  logr-server  ──▶  SQLite
                                                       ▲
                                                       │ (query socket)
                                                   logr-view (TUI)
```

- **`logr-server`** — long-running daemon. Owns the SQLite file. Listens on an _ingest socket_ (accepts records from any number of `logr-in` processes, batched through a 2-second flush ticker) and a _query socket_ (one request per connection, response stream terminated by a `done` or `error` frame). Also runs retention purge at startup and every 24 hours.
- **`logr-in`** — a forwarder. Selects one _connector_ (`stdin`, `docker`, or `cdp`), pipes raw lines through a parser, and writes records to the server's ingest socket. Buffers up to 2000 records and auto-reconnects.
- **`logr-view`** — interactive TUI (Bubble Tea + Catppuccin Mocha) for filtering, tailing, marking, and exporting log records. Persists filter and column state to `client.toml` on clean exit.

Storage is a hybrid schema: promoted columns for the four fields filters always hit (`log_time`, `source`, `level`, `message`) plus a JSON `data` text column for everything else.

```
┌──────────────────────────────────────────────────┐
│  logs table                                      │
│  id          INTEGER PRIMARY KEY AUTOINCREMENT   │
│  ingested_at TEXT      (ISO-8601, always set)    │
│  log_time    TEXT      (ISO-8601, if parsed)     │
│  source      TEXT      (service/process name)    │
│  level       TEXT      (INFO/WARN/ERROR/…)       │
│  message     TEXT      (primary text)            │
│  data        TEXT      (JSON, all other fields)  │
└──────────────────────────────────────────────────┘
```

A `REGEXP` SQLite scalar function is registered at startup so the filter DSL's `MATCHES` operator works natively.

## Prerequisites

- Go 1.26+
- Docker socket access (only if you use the `docker` connector)
- Chrome/Node with `--remote-debugging-port` (only if you use the `cdp` connector)

## Building

```bash
make          # produces bin/logr-server, bin/logr-in, bin/logr-view
make test     # go test ./...
make clean    # remove binaries

# Build a single binary without Make:
go build -o bin/logr-server ./cmd/logr-server
```

Or install the binaries into `$(go env GOPATH)/bin` without cloning:

```bash
go install github.com/roost-cc/logr/cmd/logr-server@latest
go install github.com/roost-cc/logr/cmd/logr-in@latest
go install github.com/roost-cc/logr/cmd/logr-view@latest
```

## Configuration

All three binaries read TOML from `$XDG_CONFIG_HOME/logr/` (defaults to `~/.config/logr/`):

| File          | Owner         | Keys                                                      |
| ------------- | ------------- | --------------------------------------------------------- |
| `server.toml` | `logr-server` | `db`, `ingest_socket`, `query_socket`, `retention`        |
| `ingest.toml` | `logr-in`     | `ingest_socket`, `[connector]` table                      |
| `client.toml` | `logr-view`   | `query_socket`, `filter`, `[columns]` (rewritten on exit) |

Socket addresses use a scheme prefix: `unix:///abs/path` or `tcp://host:port`. Defaults are `unix:///tmp/logr-ingest.sock` and `unix:///tmp/logr-query.sock`.

The `-c` flag accepts either a bare name (`-c servers` → `~/.config/logr/servers.toml`) or a path containing `/` (used as-is).

Write a default config with `-init`:

```bash
bin/logr-server -init                  # ~/.config/logr/server.toml
bin/logr-in -c servers -init           # ~/.config/logr/servers.toml
```

## Running `logr-server` (preferred: systemd user service)

The preferred way to run `logr-server` is as a **systemd user service**, so it starts at login and is restarted on failure without needing root.

```bash
make                              # build bin/logr-server first
scripts/install-user-service.sh   # install + daemon-reload + enable --now
```

The installed unit is equivalent to this, with `ExecStart` pointing at `bin/logr-server` in your checkout (here `~/src/logr`):

```ini
[Unit]
Description=logr-server (log ingest + query daemon)
After=default.target

[Service]
Type=simple
ExecStart=%h/src/logr/bin/logr-server
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
```

### Managing the service

```bash
systemctl --user status logr-server       # current state
systemctl --user restart logr-server      # after rebuilding
systemctl --user stop logr-server
journalctl --user -u logr-server -f       # follow logs
```

### Keep it running when logged out

User services stop at logout by default. Enable lingering once to keep `logr-server` running across SSH sessions and reboots:

```bash
loginctl enable-linger "$USER"
```

### Uninstall

```bash
scripts/install-user-service.sh --uninstall
```

Running `bin/logr-server` directly in a terminal still works for debugging.

### Server flags

| Flag       | Description                            |
| ---------- | -------------------------------------- |
| `-c <ref>` | Config file (bare name or path)        |
| `-init`    | Write a default `server.toml` and exit |
| `-migrate` | Apply the database schema and exit     |

## Running `logr-in` (the forwarder)

`logr-in` runs one connector per process. The connector type and its arguments are read from a config file; CLI flags override individual fields.

```bash
logr-in -c <name|path> [connector] [flags]
```

If no connector word is given, `cfg.Connector.Type` is used (defaulting to `stdin`).

### Stdin connector

Pipe any process through `logr-in` with `-source` to label it:

```bash
my-script.sh 2>&1 | bin/logr-in stdin -source my-script
npm run build  2>&1 | bin/logr-in stdin -source build
```

Flags: `-source <label>` (required), `-p <parser>`, `-server <addr>`.

### Docker connector

Discovers running containers via the Docker socket and tails their logs. It follows services across recreates, drops removed containers, backs off on failures and logs only state changes; see `logr-in docker --help`.

```bash
bin/logr-in docker                              # all running services
bin/logr-in docker storage account synapse      # explicit list
bin/logr-in docker -services storage,account
```

The `service-N  |` prefix added by `docker compose logs` is stripped before parsing; the original line is preserved as `data._raw`.

### CDP connector

Connects to Chrome DevTools or Node `--inspect` endpoints (both speak the same protocol) and streams console logs, runtime exceptions, and Log domain entries.

```bash
bin/logr-in cdp -endpoint http://localhost:9222
bin/logr-in cdp -endpoint http://localhost:9229 -types node
```

Without `-endpoint`, defaults to probing `localhost:9222`, `:9229`, `:9230`, `:9231`.

### Example `ingest.toml`

```toml
ingest_socket = "unix:///tmp/logr-ingest.sock"

[connector]
type     = "docker"
services = ["storage", "account", "synapse"]

# Either a single global parser…
parser = "auto"

# …or a per-service map (unmapped services use "auto"):
# [connector.parser]
# synapse   = "synapse"
# assistant = "json"
```

## Running `logr-view`

```bash
bin/logr-view                                       # uses ~/.config/logr/client.toml
bin/logr-view --filter "source=storage AND level=error"
bin/logr-view --filter 'message MATCHES "^(ERROR|FATAL)"'
```

Flags: `-c <ref>`, `-filter <expr>`.

### Keyboard shortcuts

| Key                 | Action                                                       |
| ------------------- | ------------------------------------------------------------ |
| `Tab` / `Shift+Tab` | Cycle panel focus (Filter → Sources → Columns → Logs)        |
| `j` / `↓`           | Next row / next item in focused panel                        |
| `k` / `↑`           | Previous row / previous item                                 |
| `g`                 | Jump to oldest                                               |
| `G`                 | Jump to newest (re-enables tail)                             |
| `t`                 | Toggle tail mode (auto-scroll new records)                   |
| `f`                 | Jump to filter editor                                        |
| `/`                 | Quick search — prefills `message CONTAINS `                  |
| `s`                 | Stats / aggregation panel                                    |
| `Space` / `Enter`   | Toggle source or column (in those panels) / detail (in Logs) |
| `m`                 | Mark / unmark current row                                    |
| `M`                 | Clear all marks                                              |
| `e`                 | Export (marked rows if any, otherwise all filtered rows)     |
| `r`                 | Refresh logs and source list                                 |
| `?`                 | Help overlay                                                 |
| `q` / `Ctrl+C`      | Quit                                                         |

In the Columns panel, `h`/`l` (or `←`/`→`) toggles a column's sort order between ascending, descending, and unsorted.

## Filter DSL

The filter editor (press `f`) accepts a small expression language that compiles to parameterized SQL — no injection risk.

### Field names

| Filter field    | Maps to                               |
| --------------- | ------------------------------------- |
| `source`        | `source` column                       |
| `level`         | `level` column                        |
| `message`       | `message` column                      |
| `log_time`      | `log_time` column                     |
| `ingested_at`   | `ingested_at` column                  |
| `data.foo`      | `json_extract(data, '$.foo')`         |
| `foo` (unknown) | Also rewritten to `json_extract(...)` |

### Operators

| Syntax                        | Meaning                                           |
| ----------------------------- | ------------------------------------------------- |
| `field=value`                 | Equality                                          |
| `field!=value`                | Inequality                                        |
| `field>value` / `field<value` | Comparison                                        |
| `field IN (a, b, c)`          | Set membership                                    |
| `field CONTAINS "text"`       | Case-insensitive substring match                  |
| `field MATCHES "regex"`       | Case-sensitive regex (SQLite `REGEXP`)            |
| `field MATCHES* "regex"`      | Case-insensitive regex                            |
| `field NOT MATCHES "regex"`   | Negated case-sensitive regex                      |
| `field NOT MATCHES* "regex"`  | Negated case-insensitive regex                    |
| `log_time > 10m`              | Relative time — last N seconds/minutes/hours/days |
| `AND` / `OR` / `NOT`          | Boolean combinators                               |
| `(...)`                       | Grouping                                          |

A top-level `source = X` or `source IN (...)` is extracted before the rest of the predicate is built, so source selection and content filtering compose cleanly.

### Examples

```
# Single service
source=storage

# Multiple services
source IN (storage, account)

# Errors only
level=error

# Exclude health check noise
level!=info AND NOT (message CONTAINS "health")

# Last hour of errors from any service
level=error AND log_time > 1h

# Synapse errors in the last day
source=synapse AND level=error AND log_time > 1d

# Specific HTTP status from JSON field
data.status=500

# Combine JSON and column filters
source=storage AND data.method=GET AND level=error

# Full boolean expression
(source=storage OR source=account) AND level!=info AND log_time > 2h

# Regex: messages starting with ERROR or FATAL
message MATCHES "^(ERROR|FATAL)"

# Regex: case-insensitive — any timeout mention
message MATCHES* "timeout"

# Negated regex: exclude health and metrics paths
data.path NOT MATCHES "^/(health|metrics)"
```

### Relative time units

`s` = seconds, `m` = minutes, `h` = hours, `d` = days. `log_time > 1h` means "log_time is within the last hour"; falls back to `ingested_at` when `log_time` is null.

## Stats / Aggregation Panel

Press `s` to open the stats panel. It runs live aggregate queries scoped to the **current filter**, so it acts as a log-reduce view over whatever you're looking at.

```
logr stats  filter: level=error AND log_time > 1h  total: 1,247
────────────────────────────────────────────────────────────────
BY SOURCE                       BY LEVEL
  storage     ████████  412     ERROR  ████████████  892
  account     █████     298     WARN   ████           201
  synapse     ████      187     FATAL  █               12
  assistant   ██        180
  bot         █         170
────────────────────────────────────────────────────────────────
SOURCE × LEVEL
  storage      / ERROR    ██████████  78   6%
  account      / WARN     ████        52   4%
  synapse      / ERROR    ███         41   3%
```

Each bar is scaled to the maximum in its group. Percentages are relative to the total matching the current filter. Press `r` to refresh, `Esc` (or `s`) to return.

## Column Selector

Tab into the Columns panel and toggle columns on/off with `Space` or `Enter`. Column visibility is persisted to `client.toml`. The `MESSAGE` column fills all remaining width automatically.

Default columns: `TIME`, `SOURCE`, `LEVEL`, `MESSAGE`.

To add a custom JSON column (e.g. `data.status`), add an entry to `defaultColumns` in `internal/tui/model.go`:

```go
{"data.status", "STATUS", 6, true},
```

## Export

Press `e` to export. If any rows are marked (`m`), only those are exported; otherwise all filtered rows are written.

| Format                         | Key | Output file                     |
| ------------------------------ | --- | ------------------------------- |
| JSONL (newline-delimited JSON) | `j` | `logr-export-<timestamp>.jsonl` |
| CSV                            | `c` | `logr-export-<timestamp>.csv`   |

## Log Format Parsers

The parser chain tries each parser in order and falls back to a generic catch-all:

| Parser                | Matches                                                                                         |
| --------------------- | ----------------------------------------------------------------------------------------------- |
| **JSON**              | Lines starting with `{` — extracts `time`/`ts`/`timestamp`, `level`/`severity`, `message`/`msg` |
| **Synapse / Python**  | `YYYY-MM-DD HH:MM:SS,mmm - logger - pid - LEVEL - message`                                      |
| **uvicorn / FastAPI** | `LEVEL:     ip:port - "METHOD path HTTP/1.x" status` and `LEVEL:logger:message`                 |
| **llama**             | `llama.cpp` inference logs                                                                      |
| **chromium**          | Chromium / V8 stderr lines                                                                      |
| **Node router**       | Lines matching `-> [METHOD] /path` or `<- [STATUS] /path`                                       |
| **Generic**           | Everything else — stored as `{ message: rawLine }`                                              |

Pick a specific parser per source via `parser = "json"` (global) or a `[connector.parser]` table; `"auto"` runs the chain; `"line"` (or any unknown name) wraps the raw line as a plain message.

The Docker Compose `service-N  |` prefix is stripped before parsing. The original unparsed line is always stored as `data._raw`.

Levels are normalised: `warning` → `WARN`, `critical`/`err`/`fatal`/`panic` → `ERROR`/`FATAL`, etc.

## Retention

`logr-server` deletes records older than the `retention` value in `server.toml` (default `168h` = 7 days), once at startup and then every 24 hours.

```toml
# server.toml
retention = "720h"   # 30 days
```

## Database Schema

```sql
CREATE TABLE logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ingested_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    log_time    TEXT,
    source      TEXT NOT NULL,
    level       TEXT,
    message     TEXT NOT NULL,
    data        TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX logs_log_time_idx    ON logs (log_time DESC);
CREATE INDEX logs_ingested_at_idx ON logs (ingested_at DESC);
CREATE INDEX logs_source_idx      ON logs (source);
CREATE INDEX logs_level_idx       ON logs (level);
```

You can query the database directly for ad-hoc analysis:

```sql
-- Errors in the last hour
SELECT log_time, source, message, data
FROM logs
WHERE level = 'ERROR'
  AND ingested_at > strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-1 hour')
ORDER BY ingested_at DESC;

-- Top sources by volume in the last day
SELECT source, count(*) AS n
FROM logs
WHERE ingested_at > strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-1 day')
GROUP BY source ORDER BY n DESC;

-- JSON field search
SELECT * FROM logs WHERE json_extract(data, '$.status') = '500';
```

## Project Structure

```
logr/
├── cmd/
│   ├── logr-server/    # logr-server entrypoint + query handler
│   ├── logr-in/        # logr-in entrypoint (stdin, docker, cdp connectors)
│   └── logr-view/      # logr-view (TUI) entrypoint
├── internal/
│   ├── cdp/            # Chrome DevTools Protocol websocket client
│   ├── config/         # TOML config loaders for all three binaries
│   ├── db/             # SQLite schema, batched insert, query, retention
│   ├── filter/         # Filter DSL → parameterized SQL
│   ├── parser/         # Parser chain (json, synapse, uvicorn, llama, chromium, nodeRouter, generic)
│   ├── queryclient/    # Client side of the query socket protocol
│   ├── queryproto/     # Request/Response types for the query socket
│   ├── sender/         # Async ingest writer with reconnect + bounded buffer
│   ├── tui/            # Bubble Tea model/update/view + Catppuccin Mocha styles
│   └── wire/           # Record type exchanged from logr-in → logr-server
├── scripts/
│   └── install-user-service.sh
├── bin/                # compiled binaries (gitignored)
├── go.mod
├── Makefile
└── README.md
```

## Theming

Colors are defined in `internal/tui/styles.go` using the [Catppuccin Mocha](https://catppuccin.com/palette) palette. To switch variants, replace the hex values with those from [Latte, Frappé, or Macchiato](https://catppuccin.com/palette). All color variables are at the top of that file.

## License

ISC — see [`LICENSE.md`](LICENSE.md).
