package main

import (
	"sync"

	"github.com/gorilla/websocket"
)

// routerGate couples this agent's router WebSocket connection(s) to the
// local LLM backend's health, as tracked by monitorBackendHealth (backend.go).
// While unhealthy, it force-closes every open connection and blocks new
// dials, so the router sees this agent drop off and redirects requests
// elsewhere instead of routing them into a backend that's down - a plain
// crash-restart (vllmSupervisor) or a reconnect-on-drop loop alone would
// leave the router still sending requests down a connection that answers
// with nothing but errors until the backend comes back on its own.
type routerGate struct {
	mu      sync.Mutex
	cond    *sync.Cond
	healthy bool
	conns   map[int]*websocket.Conn
}

func newRouterGate() *routerGate {
	g := &routerGate{healthy: true, conns: make(map[int]*websocket.Conn)}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// waitHealthy blocks until the backend is healthy again. Call before
// dialing, so a connection loop that just got cut for an unhealthy backend
// doesn't immediately redial straight into the same outage.
func (g *routerGate) waitHealthy() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for !g.healthy {
		g.cond.Wait()
	}
}

// setConn registers conn as connIdx's live connection so a later
// markUnhealthy can close it. clearConn removes it once that connection's
// loop moves on (drop, or a markUnhealthy close).
func (g *routerGate) setConn(connIdx int, conn *websocket.Conn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.conns[connIdx] = conn
}

func (g *routerGate) clearConn(connIdx int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.conns, connIdx)
}

// markUnhealthy force-closes every currently open router connection and
// blocks future dials until markHealthy is called.
func (g *routerGate) markUnhealthy() {
	g.mu.Lock()
	g.healthy = false
	conns := make([]*websocket.Conn, 0, len(g.conns))
	for _, c := range g.conns {
		conns = append(conns, c)
	}
	g.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
}

// markHealthy releases any connection loop blocked in waitHealthy so it
// redials.
func (g *routerGate) markHealthy() {
	g.mu.Lock()
	g.healthy = true
	g.mu.Unlock()
	g.cond.Broadcast()
}

// liveConnCount reports how many of the configured router connections are
// currently dialed - for status reporting only (reporter.go); setConn/
// clearConn remain the only writers.
func (g *routerGate) liveConnCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.conns)
}
