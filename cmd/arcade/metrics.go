package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Metrics collects HTTP request statistics.
type Metrics struct {
	startTime time.Time

	totalRequests   atomic.Int64
	unmatchedCount  atomic.Int64
	statusCounts    sync.Map // status code (int) -> *atomic.Int64
	routeCounts     sync.Map // "METHOD /pattern" (string) -> *routeMetrics
}

type routeMetrics struct {
	count        atomic.Int64
	statusCounts sync.Map // status code (int) -> *atomic.Int64
}

// MetricsSnapshot is the JSON-serializable stats output.
type MetricsSnapshot struct {
	Uptime        string                   `json:"uptime"`
	UptimeSeconds float64                  `json:"uptimeSeconds"`
	TotalRequests int64                    `json:"totalRequests"`
	Unmatched     int64                    `json:"unmatched"`
	StatusCodes   map[int]int64            `json:"statusCodes"`
	Routes        map[string]RouteSnapshot `json:"routes"`
}

// RouteSnapshot is per-route stats.
type RouteSnapshot struct {
	Count       int64         `json:"count"`
	StatusCodes map[int]int64 `json:"statusCodes"`
}

// NewMetrics creates a new Metrics collector.
func NewMetrics() *Metrics {
	return &Metrics{
		startTime: time.Now(),
	}
}

// Middleware returns a Fiber middleware that records request metrics.
func (m *Metrics) Middleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		err := c.Next()

		status := c.Response().StatusCode()
		m.totalRequests.Add(1)

		// Use the matched route pattern (e.g. "/tx/:txid") not the raw path.
		// Unmatched routes have an empty pattern — count them separately.
		pattern := c.Route().Path
		if pattern == "" {
			m.unmatchedCount.Add(1)
			return err
		}

		route := c.Method() + " " + pattern

		// Global status count
		m.incrementStatus(&m.statusCounts, status)

		// Per-route count
		val, _ := m.routeCounts.LoadOrStore(route, &routeMetrics{})
		rm := val.(*routeMetrics)
		rm.count.Add(1)
		m.incrementStatus(&rm.statusCounts, status)

		return err
	}
}

func (m *Metrics) incrementStatus(sm *sync.Map, status int) {
	val, _ := sm.LoadOrStore(status, &atomic.Int64{})
	val.(*atomic.Int64).Add(1)
}

// Snapshot returns a point-in-time copy of all metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	uptime := time.Since(m.startTime)

	snap := MetricsSnapshot{
		Uptime:        uptime.Round(time.Second).String(),
		UptimeSeconds: uptime.Seconds(),
		TotalRequests: m.totalRequests.Load(),
		Unmatched:     m.unmatchedCount.Load(),
		StatusCodes:   make(map[int]int64),
		Routes:        make(map[string]RouteSnapshot),
	}

	m.statusCounts.Range(func(k, v any) bool {
		snap.StatusCodes[k.(int)] = v.(*atomic.Int64).Load()
		return true
	})

	m.routeCounts.Range(func(k, v any) bool {
		rm := v.(*routeMetrics)
		rs := RouteSnapshot{
			Count:       rm.count.Load(),
			StatusCodes: make(map[int]int64),
		}
		rm.statusCounts.Range(func(sk, sv any) bool {
			rs.StatusCodes[sk.(int)] = sv.(*atomic.Int64).Load()
			return true
		})
		snap.Routes[k.(string)] = rs
		return true
	})

	return snap
}
