package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	dockerclient "github.com/moby/moby/client"

	"github.com/roost-cc/logr/internal/config"
	"github.com/roost-cc/logr/internal/db"
	"github.com/roost-cc/logr/internal/reconnect"
)

const dockerUsage = `logr-in docker — attach to Docker Compose container log streams

Discovers running containers via the Docker socket and tails their logs.

Lifecycle:
  - Follows services across recreates: a new container for a watched
    service is attached from its start event under the same source.
  - A stopped container is detached with one "stopped" line and reattached
    on its next start event, resuming after the last line already read.
  - A removed container is dropped with one "removed" line.
  - Transient failures (daemon restart, dropped stream) retry with backoff
    from 3s doubling to 60s. Only state changes are logged: "lost" once,
    "reconnected" on recovery, and at most one "still down" line per
    5 minutes in between.

Usage:
  logr-in docker [flags] [service ...]

Flags:
  -services <list>   Comma-separated service names to watch
                     (also accepts trailing positional args; default: all)
  -p <parser>        Parser (default: auto)
  -server <addr>     Ingest socket address

Env:
  LOGR_SERVICES   Default service list (comma-separated)
`

func runDocker(ctx context.Context, args []string, cfg config.IngestConfig) {
	fs := flag.NewFlagSet("docker", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, dockerUsage) }

	defaultServices := strings.Join(cfg.Connector.Services, ",")
	if defaultServices == "" {
		defaultServices = os.Getenv("LOGR_SERVICES")
	}

	serverAddr := fs.String("server", cfg.IngestSocket, "Ingest socket address")
	services := fs.String("services", defaultServices, "Comma-separated service names")
	parserFlag := fs.String("p", "", "Parser override for all services (default: per-service config or 'auto')")
	fs.Parse(args) //nolint:errcheck

	var serviceList []string
	for _, s := range strings.Split(*services, ",") {
		if s = strings.TrimSpace(s); s != "" {
			serviceList = append(serviceList, s)
		}
	}
	serviceList = append(serviceList, fs.Args()...)

	// resolveParser returns the parser name for a given service.
	// A -p flag overrides everything; otherwise fall back to per-service config.
	resolveParser := cfg.Connector.Parser.ForService
	if *parserFlag != "" {
		resolveParser = func(_ string) string { return *parserFlag }
	}

	snd := mustSender(ctx, *serverAddr)
	defer snd.Close()

	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		log.Fatalf("docker client: %v", err)
	}

	d := newDockerConnector(cli, serviceList, resolveParser, func(r db.Record) { snd.Send(toWire(r)) })
	if err := d.run(ctx); err != nil {
		log.Fatalf("list containers: %v", err)
	}
}

// dockerAPI is the part of the Docker client the connector uses.
// *dockerclient.Client satisfies it; tests use a fake.
type dockerAPI interface {
	ContainerList(ctx context.Context, options dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error)
	ContainerLogs(ctx context.Context, id string, options dockerclient.ContainerLogsOptions) (dockerclient.ContainerLogsResult, error)
	ContainerInspect(ctx context.Context, id string, options dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error)
	Events(ctx context.Context, options dockerclient.EventsListOptions) dockerclient.EventsResult
}

// containerGone reports whether err means the container no longer exists
// ("No such container"), as opposed to a transient failure.
func containerGone(err error) bool {
	return err != nil && cerrdefs.IsNotFound(err)
}

var errStreamEnded = errors.New("log stream ended")

// attachment is one container logr has seen. Attachments are keyed by
// container ID, not service: replicas each get their own, and service names
// can repeat across compose projects.
type attachment struct {
	id      string
	service string
	// lastEnd is when the previous log stream ended; zero before the first.
	// A reattach passes it as Since so ingested lines are not re-sent.
	lastEnd   time.Time
	streaming bool
	// startSeen records a start event that arrived while streaming. follow
	// clears it before inspecting; if it is set when an inspect reports the
	// container stopped, the container restarted in between.
	startSeen bool
	cancel    context.CancelFunc
	state     *reconnect.State
}

type dockerConnector struct {
	api           dockerAPI
	services      []string
	resolveParser func(string) string
	emit          func(db.Record)
	clock         reconnect.Clock
	logf          func(format string, args ...any)

	mu   sync.Mutex
	atts map[string]*attachment
	wg   sync.WaitGroup
}

func newDockerConnector(api dockerAPI, services []string, resolveParser func(string) string, emit func(db.Record)) *dockerConnector {
	return &dockerConnector{
		api:           api,
		services:      services,
		resolveParser: resolveParser,
		emit:          emit,
		clock:         reconnect.RealClock,
		logf:          log.Printf,
		atts:          map[string]*attachment{},
	}
}

func containerEventOptions() dockerclient.EventsListOptions {
	return dockerclient.EventsListOptions{Filters: make(dockerclient.Filters).
		Add("type", string(events.ContainerEventType)).
		Add("event", string(events.ActionStart), string(events.ActionDie), string(events.ActionDestroy)),
	}
}

// run attaches to every running allowed container, then follows container
// lifecycle events until ctx ends. It returns an error only if the initial
// container list fails.
func (d *dockerConnector) run(ctx context.Context) error {
	// Subscribe before listing so a container started in between is not missed.
	evts := d.api.Events(ctx, containerEventOptions())
	if err := d.attachRunning(ctx); err != nil {
		return err
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.watchEvents(ctx, evts.Messages, evts.Err)
	}()
	d.wg.Wait()
	return nil
}

// attachRunning attaches every running allowed container not already streaming.
func (d *dockerConnector) attachRunning(ctx context.Context) error {
	cs, err := listContainers(ctx, d.api, d.services)
	if err != nil {
		return err
	}
	for _, c := range cs {
		d.attach(ctx, c.ID, containerService(c.Labels))
	}
	return nil
}

// attach starts following a container's logs unless it is already streaming.
func (d *dockerConnector) attach(ctx context.Context, id, svc string) {
	d.mu.Lock()
	a := d.atts[id]
	if a == nil {
		a = &attachment{
			id:      id,
			service: svc,
			state:   reconnect.NewWith(fmt.Sprintf("docker: %s %s", svc, shortID(id)), d.logf, d.clock),
		}
		d.atts[id] = a
	}
	if a.streaming {
		a.startSeen = true
		d.mu.Unlock()
		return
	}
	a.streaming = true
	actx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	d.mu.Unlock()

	d.logf("docker: %s %s attached", svc, shortID(id))
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer cancel()
		d.follow(actx, a, d.resolveParser(svc))
	}()
}

// follow streams a container's logs, reattaching after transient drops, until
// the container stops, is removed, or ctx ends.
func (d *dockerConnector) follow(ctx context.Context, a *attachment, parserName string) {
	for {
		d.mu.Lock()
		since := a.lastEnd
		d.mu.Unlock()

		opts := dockerclient.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Follow: true}
		if !since.IsZero() {
			opts.Since = since.Format(time.RFC3339Nano)
		}
		rc, err := d.api.ContainerLogs(ctx, a.id, opts)
		if ctx.Err() != nil {
			if rc != nil {
				rc.Close()
			}
			return
		}
		if containerGone(err) {
			d.drop(a)
			return
		}
		if err == nil {
			a.state.Succeeded()
			err = d.stream(ctx, rc, a.service, parserName)
			if ctx.Err() != nil {
				return
			}
			// Taken before inspecting, so lines written between now and a
			// reattach are not skipped by Since.
			d.mu.Lock()
			a.lastEnd = d.clock.Now()
			d.mu.Unlock()
		}

		d.mu.Lock()
		a.startSeen = false
		d.mu.Unlock()
		res, ierr := d.api.ContainerInspect(ctx, a.id, dockerclient.ContainerInspectOptions{})
		info := res.Container
		if ctx.Err() != nil {
			return
		}
		if containerGone(ierr) {
			d.drop(a)
			return
		}
		if ierr == nil && info.State != nil && !info.State.Running {
			if d.detach(a) {
				return
			}
			continue // restarted since the inspect: resume after lastEnd
		}

		// Still running (or the daemon did not answer): a transient drop.
		switch {
		case ierr != nil:
			err = ierr
		case err == nil:
			err = errStreamEnded
		}
		a.state.Failed(err)
		if !a.state.Wait(ctx) {
			return
		}
	}
}

// stream ingests one log stream until it ends. It returns the read error, if any.
func (d *dockerConnector) stream(ctx context.Context, rc io.ReadCloser, source, parserName string) error {
	defer rc.Close()
	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, rc)
		pw.CloseWithError(err)
	}()
	err := ingestReader(ctx, pr, source, parserName, d.emit)
	pr.Close() // unblock the copier if we stopped reading early
	return err
}

// drop forgets a container that no longer exists.
func (d *dockerConnector) drop(a *attachment) {
	d.mu.Lock()
	owned := d.atts[a.id] == a
	if owned {
		delete(d.atts, a.id)
	}
	d.mu.Unlock()
	if owned {
		d.logf("docker: %s %s removed", a.service, shortID(a.id))
	}
}

// detach marks a stopped container as not streaming. Its entry (and lastEnd)
// is kept so a start event reattaches without replaying history.
//
// If a start event arrived since follow's inspect, attach saw the container
// still streaming and ignored it; detaching would leave the restarted
// container unfollowed. detach then keeps it streaming and returns false so
// follow carries on.
func (d *dockerConnector) detach(a *attachment) bool {
	d.mu.Lock()
	a.state.Reset()
	restarted := a.startSeen
	a.startSeen = false
	a.streaming = restarted
	d.mu.Unlock()
	d.logf("docker: %s %s stopped", a.service, shortID(a.id))
	if restarted {
		d.logf("docker: %s %s attached", a.service, shortID(a.id))
	}
	return !restarted
}

// destroyed handles a destroy event: the entry is deleted with a single
// "removed" line, and if it was streaming its goroutine is cancelled.
func (d *dockerConnector) destroyed(id string) {
	d.mu.Lock()
	a := d.atts[id]
	if a == nil {
		d.mu.Unlock()
		return
	}
	delete(d.atts, id)
	wasStreaming, cancel := a.streaming, a.cancel
	d.mu.Unlock()
	d.logf("docker: %s %s removed", a.service, shortID(id))
	if wasStreaming {
		cancel()
	}
}

func (d *dockerConnector) handleEvent(ctx context.Context, evt events.Message) {
	if evt.Type != "" && evt.Type != events.ContainerEventType {
		return
	}
	switch evt.Action {
	case events.ActionStart:
		svc := containerService(evt.Actor.Attributes)
		if serviceAllowed(svc, d.services) {
			d.attach(ctx, evt.Actor.ID, svc)
		}
	case events.ActionDestroy:
		d.destroyed(evt.Actor.ID)
	}
	// die needs no action: the log stream ends and follow() inspects the
	// container, which is then stopped.
}

// watchEvents follows container lifecycle events, resubscribing with backoff
// when the stream fails. After a resubscribe it re-lists running containers
// to pick up any start events missed while down.
func (d *dockerConnector) watchEvents(ctx context.Context, evtCh <-chan events.Message, errCh <-chan error) {
	st := reconnect.NewWith("docker: events", d.logf, d.clock)
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errCh:
			if ctx.Err() != nil {
				return
			}
			for {
				st.Failed(err)
				if !st.Wait(ctx) {
					return
				}
				evts := d.api.Events(ctx, containerEventOptions())
				evtCh, errCh = evts.Messages, evts.Err
				if err = d.attachRunning(ctx); err == nil {
					break
				}
				if ctx.Err() != nil {
					return
				}
			}
			st.Succeeded()
		case evt := <-evtCh:
			d.handleEvent(ctx, evt)
		}
	}
}

// listContainers returns the running containers whose service is allowed.
func listContainers(ctx context.Context, api dockerAPI, serviceList []string) ([]container.Summary, error) {
	// Fetch all running containers. Multiple label=key=value filters are ANDed
	// by the Docker API, so we can't use them to match any-of a service list.
	// Filter by service name in Go instead.
	f := make(dockerclient.Filters).Add("status", "running")
	res, err := api.ContainerList(ctx, dockerclient.ContainerListOptions{Filters: f})
	if err != nil {
		return nil, err
	}
	var result []container.Summary
	for _, c := range res.Items {
		if serviceAllowed(containerService(c.Labels), serviceList) {
			result = append(result, c)
		}
	}
	return result, nil
}

func containerService(labels map[string]string) string {
	if s := labels["com.docker.compose.service"]; s != "" {
		return s
	}
	if n := labels["name"]; n != "" {
		return n
	}
	return "unknown"
}

func serviceAllowed(name string, list []string) bool {
	if len(list) == 0 {
		return true
	}
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
