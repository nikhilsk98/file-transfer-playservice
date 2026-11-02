// Package metrics holds small process-wide counters and gauges, and periodically prints them
// as one JSON line in CloudWatch Embedded Metric Format (EMF). ECS ships stdout to CloudWatch
// Logs with the awslogs driver, and CloudWatch Logs turns EMF lines into custom metrics
// (namespace FileTransfer) without an agent or extra IAM permissions.
//
// Added for the CS6650 midterm experiments: the service otherwise has no way to show retries,
// overload drops or in-flight counts, and the NLB exposes no per-request metric.
package metrics

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Namespace is the CloudWatch namespace the EMF metrics land in.
const Namespace = "FileTransfer"

// Counter is a monotonically increasing count; each report prints the change since the last.
type Counter struct {
	value atomic.Int64
	last  int64
}

// Add increases the counter by n.
func (c *Counter) Add(n int64) { c.value.Add(n) }

// Inc increases the counter by one.
func (c *Counter) Inc() { c.value.Add(1) }

// Total returns the value since the process started.
func (c *Counter) Total() int64 { return c.value.Load() }

// Gauge is a current level (for example requests in flight). Each report prints the highest
// value seen during the interval, so short spikes between reports are not lost.
type Gauge struct {
	value atomic.Int64
	max   atomic.Int64
}

// Add changes the gauge by n (use -1 to decrement) and records a new interval maximum.
func (g *Gauge) Add(n int64) {
	v := g.value.Add(n)
	for {
		m := g.max.Load()
		if v <= m || g.max.CompareAndSwap(m, v) {
			return
		}
	}
}

// Value returns the current level.
func (g *Gauge) Value() int64 { return g.value.Load() }

// Registry is a named set of counters and gauges for one service.
type Registry struct {
	mu       sync.Mutex
	service  string
	counters map[string]*Counter
	gauges   map[string]*Gauge
}

// NewRegistry creates an empty registry; service becomes the EMF "Service" dimension.
func NewRegistry(service string) *Registry {
	return &Registry{service: service, counters: map[string]*Counter{}, gauges: map[string]*Gauge{}}
}

// Counter returns the counter called name, creating it on first use.
func (r *Registry) Counter(name string) *Counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.counters[name]; ok {
		return c
	}
	c := &Counter{}
	r.counters[name] = c
	return c
}

// Gauge returns the gauge called name, creating it on first use.
func (r *Registry) Gauge(name string) *Gauge {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.gauges[name]; ok {
		return g
	}
	g := &Gauge{}
	r.gauges[name] = g
	return g
}

// Snapshot builds one EMF document with the counter deltas and gauge maxima since the last
// snapshot, and resets the interval maxima.
func (r *Registry) Snapshot(now time.Time) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	doc := map[string]any{"Service": r.service, "TaskHost": hostname()}
	var defs []map[string]string
	names := make([]string, 0, len(r.counters)+len(r.gauges))
	for name := range r.counters {
		names = append(names, name)
	}
	for name := range r.gauges {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if c, ok := r.counters[name]; ok {
			v := c.value.Load()
			doc[name] = v - c.last
			c.last = v
			defs = append(defs, map[string]string{"Name": name, "Unit": "Count"})
		} else {
			g := r.gauges[name]
			cur := g.value.Load()
			doc[name] = g.max.Swap(cur) // report the interval max, start the next interval at the current level
			defs = append(defs, map[string]string{"Name": name, "Unit": "Count"})
		}
	}
	doc["_aws"] = map[string]any{
		"Timestamp": now.UnixMilli(),
		"CloudWatchMetrics": []map[string]any{{
			"Namespace":  Namespace,
			"Dimensions": [][]string{{"Service"}},
			"Metrics":    defs,
		}},
	}
	return doc
}

// Start prints a snapshot to stdout every interval until the process exits.
func (r *Registry) Start(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for now := range ticker.C {
			line, err := json.Marshal(r.Snapshot(now))
			if err == nil {
				fmt.Println(string(line))
			}
		}
	}()
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// Quiet reports whether per-datagram debug logging is turned off (env QUIET=1). The counters
// above replace those lines, which would otherwise write one log line per lost datagram.
func Quiet() bool {
	return os.Getenv("QUIET") == "1" || os.Getenv("QUIET") == "true"
}
