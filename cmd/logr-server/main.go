// logr-server listens on two sockets:
//
//   - ingest socket: receives newline-delimited JSON records from logr-in
//   - query socket:  handles request/response queries from logr-view
//
// Config is read from $XDG_CONFIG_HOME/logr/server.toml (created with defaults
// on first run if absent).
//
// Usage:
//
//	logr-server                    # load config from default location
//	logr-server -c server          # load ~/.config/logr/server.toml
//	logr-server -c /path/to/s.toml # load explicit file
//	logr-server --migrate          # apply schema and exit
//	logr-server --init             # write default config file and exit
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/roost-cc/logr/internal/config"
	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/wire"
)

func main() {
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `logr-server — log aggregation server

Listens on two sockets:
  ingest  receives newline-delimited JSON from logr-in
  query   answers request/response queries from logr-view

Usage:
  logr-server [flags]

Flags:
  -c <name|path>   Config file. A bare name expands to
                   $XDG_CONFIG_HOME/logr/<name>.toml
                   (default: ~/.config/logr/server.toml)
  -init            Write a default server.toml and exit
  -migrate         Apply the database schema and exit

Config keys (server.toml):
  db            = path to SQLite database file
  ingest_socket = address for log ingestion  (unix:// or tcp://)
  query_socket  = address for viewer queries (unix:// or tcp://)
  retention     = how long to keep logs (e.g. "168h")
`)
	}

	cfgFlag := flag.String("c", "", "Config file (name or path)")
	migrate := flag.Bool("migrate", false, "Apply schema migration and exit")
	initConf := flag.Bool("init", false, "Write default server.toml and exit")
	flag.Parse()

	cfgPath := ""
	if *cfgFlag != "" {
		cfgPath = config.Resolve(*cfgFlag)
	}

	if *initConf {
		if err := config.WriteServerTemplate(cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "init: %v\n", err)
			os.Exit(1)
		}
		if cfgPath == "" {
			cfgPath = serverConfigPath()
		}
		fmt.Printf("wrote %s\n", cfgPath)
		return
	}

	cfg, err := config.LoadServer(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Ensure DB directory exists
	if err := os.MkdirAll(dirOf(cfg.DB), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir db: %v\n", err)
		os.Exit(1)
	}

	sqlDB, err := db.Open(cfg.DB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer sqlDB.Close()

	if err := db.Migrate(ctx, sqlDB); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if *migrate {
		fmt.Println("Schema applied.")
		return
	}

	retention, err := time.ParseDuration(cfg.Retention)
	if err != nil {
		log.Printf("bad retention %q, defaulting to 168h: %v", cfg.Retention, err)
		retention = 168 * time.Hour
	}

	ins := db.NewInserter(sqlDB)
	var wg sync.WaitGroup

	// Retention goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		runRetention(ctx, sqlDB, retention)
	}()

	// Flush ticker
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				ins.Flush(context.Background()) //nolint:errcheck
				return
			case <-tick.C:
				if err := ins.Flush(ctx); err != nil {
					log.Printf("flush: %v", err)
				}
			}
		}
	}()

	// Ingest socket
	ingestLn, err := listenAddr(cfg.IngestSocket)
	if err != nil {
		log.Fatalf("ingest listen %s: %v", cfg.IngestSocket, err)
	}
	log.Printf("server: ingest  → %s", cfg.IngestSocket)
	wg.Add(1)
	go func() {
		defer wg.Done()
		ingestLoop(ctx, ingestLn, ins)
	}()
	go func() { <-ctx.Done(); ingestLn.Close() }()

	// Query socket
	queryLn, err := listenAddr(cfg.QuerySocket)
	if err != nil {
		log.Fatalf("query listen %s: %v", cfg.QuerySocket, err)
	}
	log.Printf("server: query   → %s", cfg.QuerySocket)
	wg.Add(1)
	go func() {
		defer wg.Done()
		queryLoop(ctx, queryLn, sqlDB)
	}()
	go func() { <-ctx.Done(); queryLn.Close() }()

	wg.Wait()
}

// listenAddr parses "unix:///path" or "tcp://host:port" and returns a listener.
func listenAddr(addr string) (net.Listener, error) {
	network, hostaddr, err := parseAddr(addr)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		os.Remove(hostaddr)
	}
	return net.Listen(network, hostaddr)
}

func ingestLoop(ctx context.Context, ln net.Listener, ins *db.Inserter) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("ingest accept: %v", err)
			return
		}
		go handleIngest(ctx, conn, ins)
	}
}

func handleIngest(ctx context.Context, conn net.Conn, ins *db.Inserter) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		var w wire.Record
		if err := json.Unmarshal(scanner.Bytes(), &w); err != nil {
			log.Printf("ingest: bad record: %v", err)
			continue
		}
		r := db.Record{
			Source:  w.Source,
			Level:   w.Level,
			Message: w.Message,
			LogTime: w.LogTime,
			Data:    w.Data,
		}
		if err := ins.Add(ctx, r); err != nil && ctx.Err() == nil {
			log.Printf("ingest: insert: %v", err)
		}
	}
}

func queryLoop(ctx context.Context, ln net.Listener, sqlDB *sql.DB) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("query accept: %v", err)
			return
		}
		go serveQueryConn(ctx, conn, sqlDB)
	}
}

func runRetention(ctx context.Context, sqlDB *sql.DB, retention time.Duration) {
	purge := func() {
		n, err := db.Purge(ctx, sqlDB, retention)
		if err != nil && ctx.Err() == nil {
			log.Printf("retention: %v", err)
		} else if n > 0 {
			log.Printf("retention: purged %d rows older than %v", n, retention)
		}
	}
	purge()
	tick := time.NewTicker(24 * time.Hour)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			purge()
		}
	}
}

func parseAddr(addr string) (network, hostaddr string, err error) {
	switch {
	case len(addr) > 7 && addr[:7] == "unix://":
		return "unix", addr[7:], nil
	case len(addr) > 6 && addr[:6] == "tcp://":
		return "tcp", addr[6:], nil
	}
	return "", "", fmt.Errorf("unknown scheme in %q (want unix:// or tcp://)", addr)
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func serverConfigPath() string {
	// Duplicate of config.serverPath — avoids importing an unexported symbol.
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		xdg = home + "/.config"
	}
	return xdg + "/logr/server.toml"
}
