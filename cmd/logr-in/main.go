// logr-in reads logs from a connector and forwards them to logr-server.
//
// A connector config file specifies which connector to run, its arguments,
// and which ingest socket to write to. CLI flags override any config value.
//
// Usage:
//
//	logr-in -c <name|path> [connector] [flags]
//	some-cmd | logr-in -c servers
//	logr-in -c servers docker
//
// Connectors:
//
//	stdin   Read from stdin
//	docker  Attach to Docker Compose container log streams
//	cdp     Connect to Chrome/Node.js via DevTools Protocol
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/roost-cc/logr/internal/cdp"
	"github.com/roost-cc/logr/internal/config"
	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/parser"
	"github.com/roost-cc/logr/internal/sender"
	"github.com/roost-cc/logr/internal/wire"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ", ") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

const topHelp = `logr-in — log ingestion forwarder

Reads logs from a connector and forwards them to logr-server's ingest socket.
The connector type and its arguments can be set in a config file; CLI flags
always take precedence.

Usage:
  logr-in -c <name|path> [connector] [flags]
  some-cmd | logr-in -c <name|path>

Connectors:
  stdin   Read from stdin (default)
  docker  Attach to Docker Compose container log streams
  cdp     Connect to Chrome/Node.js via DevTools Protocol

Global flags:
  -c <name|path>   Ingest config file. A bare name expands to
                   $XDG_CONFIG_HOME/logr/<name>.toml
                   (default: ~/.config/logr/ingest.toml)
  -init            Write a template config at the -c path and exit
  -h, --help       Show this help

Run 'logr-in <connector> --help' for connector-specific flags.

Example config (~/.config/logr/servers.toml):
  ingest_socket = "unix:///tmp/logr-ingest.sock"

  [connector]
  type     = "docker"
  parser   = "auto"
  services = ["storage", "account", "synapse"]
`

func main() {
	// Intercept top-level help before a connector word.
	// logr-in -h         → top-level help
	// logr-in docker -h  → docker connector help
	for _, a := range os.Args[1:] {
		if !strings.HasPrefix(a, "-") {
			break
		}
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprint(os.Stderr, topHelp)
			os.Exit(0)
		}
	}

	// Pre-scan for -c so it works before or after the connector name.
	cfgVal, args := extractFlag(os.Args[1:], "c")
	cfgPath := ""
	if cfgVal != "" {
		cfgPath = config.Resolve(cfgVal)
	}

	// Handle -init before loading (the file may not exist yet).
	for i, a := range args {
		if a == "-init" || a == "--init" {
			args = append(args[:i:i], args[i+1:]...)
			path, err := config.WriteIngestTemplate(cfgPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "init: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("wrote %s\n", path)
			return
		}
	}

	cfg, err := config.LoadIngest(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	// Connector type: explicit arg overrides config.
	connector := cfg.Connector.Type
	if connector == "" {
		connector = "stdin"
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		connector = args[0]
		args = args[1:]
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	switch connector {
	case "stdin":
		runStdin(ctx, args, cfg)
	case "docker":
		runDocker(ctx, args, cfg)
	case "cdp":
		runCDP(ctx, args, cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown connector %q  (want: stdin, docker, cdp)\n", connector)
		os.Exit(1)
	}
}

// ---- stdin ----

func runStdin(ctx context.Context, args []string, cfg config.IngestConfig) {
	fs := flag.NewFlagSet("stdin", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `logr-in stdin — read logs from stdin

Usage:
  some-cmd | logr-in [flags]
  some-cmd | logr-in stdin [flags]

Flags:
  -source <label>   Source name for ingested logs (required)
  -p <parser>       Parser to apply (default: line)
  -server <addr>    Ingest socket address

Parsers:
  line      treat each line as a plain message (default)
  json      parse structured JSON logs
  auto      try parsers in order, fall back to line
  uvicorn   uvicorn/FastAPI access logs
  nginx     nginx access logs
  synapse   Matrix Synapse / Python logging format
  llama     llama.cpp inference logs
  chromium  Chromium stderr logs
`)
	}

	serverAddr := fs.String("server", cfg.IngestSocket, "Ingest socket address")
	source := fs.String("source", cfg.Connector.Source, "Source label (required)")
	parserFlag := fs.String("p", "", "Parser override (default: from config or 'line')")
	fs.Parse(args) //nolint:errcheck

	if *source == "" {
		fmt.Fprintln(os.Stderr, "logr-in stdin: -source is required")
		os.Exit(1)
	}

	parserName := *parserFlag
	if parserName == "" {
		parserName = cfg.Connector.Parser.ForService(*source)
	}

	snd := mustSender(ctx, *serverAddr)
	defer snd.Close()

	ingestReader(ctx, os.Stdin, *source, parserName, func(r db.Record) { snd.Send(toWire(r)) }) //nolint:errcheck
}

// ---- cdp ----

func runCDP(ctx context.Context, args []string, cfg config.IngestConfig) {
	fs := flag.NewFlagSet("cdp", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `logr-in cdp — ingest Chrome/Node.js logs via DevTools Protocol

Connects to CDP debug endpoints and streams console logs, runtime
exceptions, and Log domain entries.

Usage:
  logr-in cdp [flags]

Flags:
  -endpoint <url>   CDP endpoint URL, repeatable
                    (default: localhost:9222, :9229, :9230, :9231)
  -types <list>     Target types to connect to: page, node, worker, iframe
                    (default: all non-internal targets)
  -server <addr>    Ingest socket address

Examples:
  logr-in cdp -endpoint http://localhost:9222
  logr-in cdp -endpoint http://localhost:9229 -types node
`)
	}

	defaultTypes := strings.Join(cfg.Connector.Types, ",")

	serverAddr := fs.String("server", cfg.IngestSocket, "Ingest socket address")
	var endpoints multiFlag
	for _, ep := range cfg.Connector.Endpoints {
		endpoints = append(endpoints, ep)
	}
	fs.Var(&endpoints, "endpoint", "CDP endpoint URL (repeatable)")
	types := fs.String("types", defaultTypes, "Target types: page, node, worker, iframe")
	fs.Parse(args) //nolint:errcheck

	if len(endpoints) == 0 {
		endpoints = multiFlag{
			"http://localhost:9222",
			"http://localhost:9229",
			"http://localhost:9230",
			"http://localhost:9231",
		}
	}

	var targetTypes []string
	for _, t := range strings.Split(*types, ",") {
		if t = strings.TrimSpace(t); t != "" {
			targetTypes = append(targetTypes, t)
		}
	}

	snd := mustSender(ctx, *serverAddr)
	defer snd.Close()

	emit := func(r db.Record) { snd.Send(toWire(r)) }

	var wg sync.WaitGroup
	for _, ep := range endpoints {
		ep := ep
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("cdp: watching %s", ep)
			cdp.WatchEndpoint(ctx, ep, emit, targetTypes)
		}()
	}
	wg.Wait()
}

// ---- helpers ----

func mustSender(ctx context.Context, addr string) *sender.Sender {
	snd, err := sender.New(ctx, addr)
	if err != nil {
		log.Fatalf("sender: %v", err)
	}
	return snd
}

func toWire(r db.Record) wire.Record {
	return wire.Record{
		Source:  r.Source,
		Level:   r.Level,
		Message: r.Message,
		LogTime: r.LogTime,
		Data:    r.Data,
	}
}

// ingestReader parses and emits each line of r until EOF, a read error or ctx
// ends. It returns the read error, if any.
func ingestReader(ctx context.Context, r io.Reader, source, parserName string, emit func(db.Record)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		raw := scanner.Text()
		var rec db.Record
		if parserName == "line" || parserName == "" {
			rec = parser.ParseLine(source, raw)
		} else {
			rec = parser.ParseWith(parserName, source, raw)
		}
		emit(rec)
	}
	return scanner.Err()
}

// orDefault returns s if non-empty, otherwise fallback.
func orDefault(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// extractFlag removes -name VALUE or -name=VALUE from args (single or double dash).
func extractFlag(args []string, name string) (value string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		for _, prefix := range []string{"-" + name + "=", "--" + name + "="} {
			if strings.HasPrefix(a, prefix) {
				return a[len(prefix):], append(args[:i:i], args[i+1:]...)
			}
		}
		if a == "-"+name || a == "--"+name {
			if i+1 < len(args) {
				return args[i+1], append(args[:i:i], args[i+2:]...)
			}
		}
	}
	return "", args
}
