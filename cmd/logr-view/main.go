// logr-view is a TUI log viewer. It connects to logr-server's query socket
// rather than opening the database directly.
//
// Config: $XDG_CONFIG_HOME/logr/client.toml
//
// Usage:
//
//	logr-view
//	logr-view -c client                    # load ~/.config/logr/client.toml
//	logr-view -c /path/to/client.toml      # load explicit file
//	logr-view --filter 'source=storage AND level=error'
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/roost-cc/logr/internal/config"
	"github.com/roost-cc/logr/internal/queryclient"
	"github.com/roost-cc/logr/internal/tui"
)

func main() {
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `logr-view — interactive TUI log viewer

Connects to logr-server's query socket. Filter and column state
are persisted to the client config on exit.

Usage:
  logr-view [flags]

Flags:
  -c <name|path>   Config file. A bare name expands to
                   $XDG_CONFIG_HOME/logr/<name>.toml
                   (default: ~/.config/logr/client.toml)
  -filter <expr>   Initial filter expression (overrides config)

Config keys (client.toml):
  query_socket = logr-server query address  (unix:// or tcp://)
  filter       = saved filter expression
  [columns]    = per-column visibility (log_time, source, level, message)

Filter examples:
  source=storage
  level=error AND log_time > 1h
  message CONTAINS "timeout"
  data.status=500
`)
	}

	cfgFlag := flag.String("c", "", "Config file (name or path)")
	filterExpr := flag.String("filter", "", "Initial filter expression (overrides config)")
	flag.Parse()

	cfgPath := ""
	if *cfgFlag != "" {
		cfgPath = config.Resolve(*cfgFlag)
	}

	cfg, err := config.LoadClient(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	qc := queryclient.New(cfg.QuerySocket)

	m := tui.NewModel(qc, cfg)
	if *filterExpr != "" {
		if err := m.SetFilter(*filterExpr); err != nil {
			fmt.Fprintf(os.Stderr, "invalid filter: %v\n", err)
			os.Exit(1)
		}
	}

	p := tea.NewProgram(m)
	final, err := p.Run()
	if err != nil {
		log.Fatal(err)
	}
	// Model is a value type: the state to persist is in the model Run returns,
	// not in m.
	if fm, ok := final.(tui.Model); ok {
		m = fm
	}

	// Persist view state on clean exit
	cfg.Filter = m.FilterExpr()
	cfg.Columns = m.ColumnState()
	if err := config.SaveClient(cfg, cfgPath); err != nil {
		log.Printf("save config: %v", err)
	}
}
