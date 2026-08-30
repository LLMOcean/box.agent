package main

import (
	"log"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/LLMOcean/box.agent/version"
)

// sharedVLLMSupervisor holds a *vllmSupervisor that's only assigned once,
// partway through main.go's startup (inside the -deploy-vllm block, after
// ensureVLLMInstalled/downloadModel succeed) - reportStatusLoop's goroutine
// is already running by that point (started before -deploy-vllm, so it can
// report download/boot progress live), so it needs a race-free way to pick
// up that assignment rather than reading a plain *vllmSupervisor variable
// concurrently with main()'s write to it. atomic.Value rather than a mutex,
// matching sharedHealth/sharedBackendState (backend.go) - a plain load/
// store is all a single pointer needs. get() returns nil both before it's
// ever set and if explicitly set to a nil *vllmSupervisor (the non-
// -deploy-vllm case) - callers treat both the same way already.
type sharedVLLMSupervisor struct {
	v atomic.Value // holds *vllmSupervisor
}

func (s *sharedVLLMSupervisor) set(sup *vllmSupervisor) {
	s.v.Store(sup)
}

func (s *sharedVLLMSupervisor) get() *vllmSupervisor {
	sup, _ := s.v.Load().(*vllmSupervisor)
	return sup
}

// reportStatusLoop sends a periodic host/backend/vllm/router-connection
// snapshot to the management API - descriptive state, not alerting; see
// reportHealth (backend.go/api.go) for the low-latency transition-alerting
// path this complements rather than replaces.
//
// Two things trigger a send: an immediate, unjittered report at startup (a
// single request, no thundering-herd risk, so the server has fresh data
// right after a restart instead of waiting a full interval), and then
// forever after, both a steady-state heartbeat every interval (after a
// random 0..interval jittered delay, to avoid many instances deployed
// around the same time ticking in lockstep) and an immediate out-of-band
// send whenever notify fires (backend.go's monitorBackendHealth or
// main.go's -deploy-vllm block, on every BackendState transition) - this
// second path is what makes "watch it download/boot live" work, since the
// 60s default heartbeat alone would be too slow for that.
func reportStatusLoop(api *apiClient, backend *llmBackend, vllmSup *sharedVLLMSupervisor, gate *routerGate, health *sharedHealth, state *sharedBackendState, backendType, model string, configuredConns int, interval time.Duration, processStartedAt time.Time, notify <-chan struct{}) {
	send := func() {
		req := reportStatusRequest{
			State:              state.get(),
			AgentVersion:       version.Version,
			AgentUptimeSeconds: time.Since(processStartedAt).Seconds(),
			Host:               collectHostStatus(),
			Backend: BackendStatus{
				Type:    backendType,
				Model:   model,
				Healthy: health.get(),
			},
			RouterConnections:           gate.liveConnCount(),
			RouterConnectionsConfigured: configuredConns,
			ReportedAt:                  time.Now(),
		}
		if backendType == "vllm" {
			if m, err := scrapeVLLMMetrics(backend.baseURL); err == nil {
				req.Backend.VLLM = m
			}
		}
		if sup := vllmSup.get(); sup != nil {
			st := sup.status()
			req.VLLMProcess = &st
		}
		if err := api.reportStatus(req); err != nil {
			log.Printf("failed to report status: %v", err)
		}
	}

	send() // immediate, unjittered

	jitter := time.Duration(rand.Int63n(int64(interval)))
	timer := time.NewTimer(jitter)
	defer timer.Stop()

	for {
		select {
		case <-notify:
			send()
		case <-timer.C:
			send()
			timer.Reset(interval)
		}
	}
}

// reportMetricsLoop flushes and reports requestMetrics on a fixed window,
// unconditionally - a zero-traffic window is itself meaningful (see
// reportMetricsRequest's field docs, api.go), so it's never skipped.
func reportMetricsLoop(api *apiClient, metrics *requestMetrics, window time.Duration) {
	jitter := time.Duration(rand.Int63n(int64(window)))
	time.Sleep(jitter)

	windowStart := time.Now()
	ticker := time.NewTicker(window)
	defer ticker.Stop()

	for range ticker.C {
		w := metrics.swap()
		now := time.Now()
		req := w.snapshot(windowStart, now.Sub(windowStart).Seconds())
		windowStart = now
		if err := api.reportMetrics(req); err != nil {
			log.Printf("failed to report metrics: %v", err)
		}
	}
}
