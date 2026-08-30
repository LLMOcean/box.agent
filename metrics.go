package main

import (
	"sync/atomic"
	"time"
)

// latencyBoundariesMs are the upper edges of each histogram bucket, in
// milliseconds - shared by both the non-streaming latency and streaming
// TTFT series, so their p95 resolution is documented in one place. A
// sample greater than the last boundary falls into the final, unbounded
// overflow bucket (see numLatencyBuckets).
var latencyBoundariesMs = [10]int64{50, 100, 200, 400, 800, 1600, 3200, 6400, 12800, 25600}

// numLatencyBuckets = len(latencyBoundariesMs) + 1 (the +1 is the unbounded
// overflow bucket) - a separate constant since Go array sizes must be a
// constant expression, and latencyBoundariesMs's own len (of an array
// type) would work but this is clearer.
const numLatencyBuckets = 11

// bucketIndex returns which bucket ms falls into.
func bucketIndex(ms int64) int {
	for i, b := range latencyBoundariesMs {
		if ms <= b {
			return i
		}
	}
	return len(latencyBoundariesMs) // overflow bucket
}

// p95FromBuckets walks cumulative bucket counts until crossing 95% of
// total, returning that bucket's upper edge - a standard bucketed-histogram
// percentile approximation (the same idea Prometheus histograms use),
// accurate to bucket resolution, not an exact percentile. The right
// tradeoff here: recording a sample is a lock-free atomic increment into a
// fixed-size array (see requestWindow), not a sorted sample set, so exact
// percentiles aren't available without a lock or unbounded memory on the
// concurrent per-request hot path this feeds.
func p95FromBuckets(buckets []int64, total int64) float64 {
	if total == 0 {
		return 0
	}
	target := int64(float64(total) * 0.95)
	if target == 0 {
		target = 1
	}
	var cum int64
	for i, c := range buckets {
		cum += c
		if cum >= target {
			if i < len(latencyBoundariesMs) {
				return float64(latencyBoundariesMs[i])
			}
			break
		}
	}
	return float64(latencyBoundariesMs[len(latencyBoundariesMs)-1])
}

// requestWindow accumulates counters for one reporting window
// (reportMetricsLoop, reporter.go). Written via plain sync/atomic calls
// (go.mod pins go 1.18 - no generic atomic.Int64) from potentially many
// concurrent per-request goroutines (connection.go's handleChat spawns one
// goroutine per request); read back only after requestMetrics.swap() has
// taken a window out of circulation, so reads and writes never race on the
// same *requestWindow.
//
// int64 fields are listed first so 64-bit atomic ops stay naturally
// aligned on 32-bit platforms too (sync/atomic's own documented
// requirement), even though box-agent only ships amd64/arm64 builds today.
type requestWindow struct {
	requestCount int64
	errorCount   int64
	inputTokens  int64
	outputTokens int64

	nonStreamCount        int64
	nonStreamLatencySumMs int64
	nonStreamBuckets      [numLatencyBuckets]int64

	streamCount     int64
	streamTTFTSumMs int64
	streamBuckets   [numLatencyBuckets]int64
}

func (w *requestWindow) recordRequestStart() {
	atomic.AddInt64(&w.requestCount, 1)
}

func (w *requestWindow) recordError() {
	atomic.AddInt64(&w.errorCount, 1)
}

func (w *requestWindow) recordTokens(usage *Usage) {
	if usage == nil {
		return
	}
	atomic.AddInt64(&w.inputTokens, int64(usage.InputTokens))
	atomic.AddInt64(&w.outputTokens, int64(usage.OutputTokens))
}

func (w *requestWindow) recordNonStreamLatency(d time.Duration) {
	ms := d.Milliseconds()
	atomic.AddInt64(&w.nonStreamCount, 1)
	atomic.AddInt64(&w.nonStreamLatencySumMs, ms)
	atomic.AddInt64(&w.nonStreamBuckets[bucketIndex(ms)], 1)
}

func (w *requestWindow) recordTTFT(d time.Duration) {
	ms := d.Milliseconds()
	atomic.AddInt64(&w.streamCount, 1)
	atomic.AddInt64(&w.streamTTFTSumMs, ms)
	atomic.AddInt64(&w.streamBuckets[bucketIndex(ms)], 1)
}

// snapshot reads w's final counters into a reportMetricsRequest (api.go) -
// only ever called after swap() has taken w out of circulation, so there
// are no concurrent writers left to race with these reads.
func (w *requestWindow) snapshot(windowStart time.Time, windowSeconds float64) reportMetricsRequest {
	req := reportMetricsRequest{
		WindowStart:    windowStart,
		WindowSeconds:  windowSeconds,
		RequestCount:   atomic.LoadInt64(&w.requestCount),
		ErrorCount:     atomic.LoadInt64(&w.errorCount),
		InputTokens:    atomic.LoadInt64(&w.inputTokens),
		OutputTokens:   atomic.LoadInt64(&w.outputTokens),
		NonStreamCount: atomic.LoadInt64(&w.nonStreamCount),
		StreamCount:    atomic.LoadInt64(&w.streamCount),
	}
	if req.RequestCount > 0 {
		req.AvgInputTokensPerRequest = float64(req.InputTokens) / float64(req.RequestCount)
		req.AvgOutputTokensPerRequest = float64(req.OutputTokens) / float64(req.RequestCount)
		if windowSeconds > 0 {
			req.TokensPerSecond = float64(req.InputTokens+req.OutputTokens) / windowSeconds
		}
	}
	if req.NonStreamCount > 0 {
		req.NonStreamLatencyAvgMs = float64(atomic.LoadInt64(&w.nonStreamLatencySumMs)) / float64(req.NonStreamCount)
		req.NonStreamLatencyP95Ms = p95FromBuckets(w.nonStreamBuckets[:], req.NonStreamCount)
	}
	if req.StreamCount > 0 {
		req.StreamTTFTAvgMs = float64(atomic.LoadInt64(&w.streamTTFTSumMs)) / float64(req.StreamCount)
		req.StreamTTFTP95Ms = p95FromBuckets(w.streamBuckets[:], req.StreamCount)
	}
	return req
}

// requestMetrics holds the current, in-flight requestWindow behind an
// atomic.Value (available since Go 1.4 - no go 1.18 conflict, unlike a
// generic atomic.Pointer[T]).
type requestMetrics struct {
	current atomic.Value // holds *requestWindow
}

func newRequestMetrics() *requestMetrics {
	m := &requestMetrics{}
	m.current.Store(&requestWindow{})
	return m
}

// window returns the current window for writers to record into - a single
// atomic load, effectively free on the per-request hot path.
func (m *requestMetrics) window() *requestWindow {
	return m.current.Load().(*requestWindow)
}

// swap atomically hands the caller the current window and installs a fresh
// empty one for future writers. A write racing exactly at swap time still
// lands correctly - it either read the old window just before the swap (and
// is safely included in what's about to be reported) or the new one just
// after (and starts the next window) - so no sample is ever lost or
// double-counted.
func (m *requestMetrics) swap() *requestWindow {
	old := m.window()
	m.current.Store(&requestWindow{})
	return old
}
