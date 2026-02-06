package core

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// healthCheckTimeout is the maximum time allowed for all health probes to complete.
// If any probe exceeds this deadline, the health check returns 503 Service Unavailable.
const healthCheckTimeout = 2 * time.Second

// HealthProbe defines the interface for a subsystem health check.
// Each probe represents a critical dependency (Database, S3, SQS) that must be
// operational for the service to function correctly.
type HealthProbe interface {
	// Name returns a human-readable identifier for the probe (e.g., "database", "s3", "sqs").
	Name() string

	// Check performs the health check against the subsystem.
	// It should respect the context deadline and return an error if the subsystem
	// is unhealthy or unreachable.
	Check(ctx context.Context) error
}

// componentStatus represents the health state of a single subsystem.
type componentStatus struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// healthResponse is the JSON response body for the health check endpoint.
type healthResponse struct {
	Status     string                     `json:"status"`
	Components map[string]componentStatus `json:"components,omitempty"`
}

// HandleHealth executes all registered health probes concurrently with a short timeout.
// Returns 200 OK if all probes report healthy, 503 Service Unavailable if any
// critical subsystem fails or if the global timeout is exceeded.
//
// The handler creates a context with a 2-second deadline derived from the request
// context. Each probe executes independently in its own goroutine to minimize
// total health check latency.
//
// This endpoint is public (no authentication required) and is mounted at GET /health.
func (s *Server) HandleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
	defer cancel()

	probes := s.HealthProbes
	if len(probes) == 0 {
		// No probes registered: report healthy with no component details.
		JSON(w, r, http.StatusOK, healthResponse{Status: "healthy"})
		return
	}

	type probeResult struct {
		name string
		err  error
	}

	var (
		mu      sync.Mutex
		results = make([]probeResult, 0, len(probes))
		wg      sync.WaitGroup
	)

	for _, probe := range probes {
		wg.Add(1)
		go func(p HealthProbe) {
			defer wg.Done()

			var err error
			func() {
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("probe panicked: %v", r)
					}
				}()
				err = p.Check(ctx)
			}()

			mu.Lock()
			results = append(results, probeResult{name: p.Name(), err: err})
			mu.Unlock()
		}(probe)
	}

	// Wait for all probes to complete or context to expire.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All probes completed within the timeout.
	case <-ctx.Done():
		// Timeout expired before all probes completed. Build a partial response
		// with whatever results we have. Missing probes are marked as timed out.
	}

	// Build the response from collected results.
	mu.Lock()
	collectedResults := make([]probeResult, len(results))
	copy(collectedResults, results)
	mu.Unlock()

	// Create a lookup of completed probes.
	completed := make(map[string]probeResult, len(collectedResults))
	for _, r := range collectedResults {
		completed[r.name] = r
	}

	components := make(map[string]componentStatus, len(probes))
	allHealthy := true

	for _, probe := range probes {
		name := probe.Name()
		if result, ok := completed[name]; ok {
			if result.err != nil {
				allHealthy = false
				components[name] = componentStatus{
					Status:  "unhealthy",
					Message: result.err.Error(),
				}
			} else {
				components[name] = componentStatus{
					Status: "healthy",
				}
			}
		} else {
			// Probe did not complete before timeout.
			allHealthy = false
			components[name] = componentStatus{
				Status:  "unhealthy",
				Message: "health check timed out",
			}
		}
	}

	resp := healthResponse{
		Components: components,
	}

	if allHealthy {
		resp.Status = "healthy"
		JSON(w, r, http.StatusOK, resp)
	} else {
		resp.Status = "unhealthy"
		JSON(w, r, http.StatusServiceUnavailable, resp)
	}
}
