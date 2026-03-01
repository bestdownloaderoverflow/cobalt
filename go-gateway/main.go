package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// Configuration
const (
	GatewayPort = 9111
	WorkerCount = 5
	AuthUser    = "admin"
	AuthPass    = "secretpassword"
)

// Worker represents a cobalt-api worker instance
type Worker struct {
	ID               string
	Host             string
	APIPort          uint16
	ControlPort      uint16
	Healthy          bool
	Restarting       bool
	RestartScheduled bool
	Failures         uint32
}

// Workers is the thread-safe worker registry
type Workers struct {
	mu      sync.RWMutex
	workers []*Worker
}

// ErrorResponse represents error response from cobalt API
type ErrorResponse struct {
	Error *ErrorDetail `json:"error"`
}

// ErrorDetail represents error detail
type ErrorDetail struct {
	Code string `json:"code"`
}

var (
	workers    *Workers
	httpClient *http.Client
)

func init() {
	httpClient = &http.Client{
		Timeout:   60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 10,
		},
	}

	// Initialize workers
	workerList := make([]*Worker, 0, WorkerCount)
	for i := 1; i <= WorkerCount; i++ {
		workerList = append(workerList, &Worker{
			ID:               fmt.Sprintf("w%d", i),
			Host:             fmt.Sprintf("gluetun-%d", i),
			APIPort:          9000,
			ControlPort:      8000,
			Healthy:          true,
			Restarting:       false,
			RestartScheduled: false,
			Failures:         0,
		})
	}
	workers = &Workers{workers: workerList}
	fmt.Printf("Initialized %d workers.\n", len(workerList))
}

// parseLengthHeader parses a length header value
func parseLengthHeader(value string) *int64 {
	if value == "" {
		return nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		n = -1
	}
	return &n
}

// healthCheckWorker checks if a worker is healthy
func healthCheckWorker(host string, port uint16) bool {
	url := fmt.Sprintf("http://%s:%d/api/serverInfo", host, port)

	for attempt := 1; attempt <= 15; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			cancel()
			fmt.Printf("[%s] Health check attempt %d failed: %v\n", host, attempt, err)
			time.Sleep(3 * time.Second)
			continue
		}

		resp, err := httpClient.Do(req)
		cancel()

		if err != nil {
			fmt.Printf("[%s] Health check attempt %d failed: %v\n", host, attempt, err)
			time.Sleep(3 * time.Second)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}

// scheduleRestart marks a worker for restart
func (w *Workers) scheduleRestart(workerID string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, worker := range w.workers {
		if worker.ID == workerID {
			if !worker.Restarting && !worker.RestartScheduled {
				worker.RestartScheduled = true
				fmt.Printf("[%s] Restart scheduled after successful failover.\n", workerID)
			}
			break
		}
	}
}

// executeScheduledRestarts executes all scheduled restarts
func (w *Workers) executeScheduledRestarts() {
	w.mu.RLock()
	scheduledIDs := make([]string, 0)
	for _, worker := range w.workers {
		if worker.RestartScheduled && !worker.Restarting {
			scheduledIDs = append(scheduledIDs, worker.ID)
		}
	}
	w.mu.RUnlock()

	for _, workerID := range scheduledIDs {
		// Clear the scheduled flag first
		w.mu.Lock()
		for _, worker := range w.workers {
			if worker.ID == workerID {
				worker.RestartScheduled = false
				break
			}
		}
		w.mu.Unlock()

		// Spawn restart task
		go restartVPNWithCooldown(w, workerID)
	}
}

// restartVPNWithCooldown restarts a VPN container with cooldown
func restartVPNWithCooldown(w *Workers, workerID string) {
	// Check if already restarting
	w.mu.Lock()
	for _, worker := range w.workers {
		if worker.ID == workerID {
			if worker.Restarting {
				fmt.Printf("[%s] Already restarting, skipping.\n", workerID)
				w.mu.Unlock()
				return
			}
			worker.Restarting = true
			worker.Healthy = false
			worker.RestartScheduled = false
			break
		}
	}
	w.mu.Unlock()

	// Map worker_id to container names (w1 -> gluetun-1 & cobalt-api-1)
	gluetunName := fmt.Sprintf("gluetun-%s", workerID[1:])
	cobaltName := fmt.Sprintf("cobalt-api-%s", workerID[1:])

	fmt.Printf("[%s] HEALING: Restarting containers '%s' and '%s'...\n", workerID, gluetunName, cobaltName)

	ctx := context.Background()
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Printf("[%s] HEAL FAILED: Failed to connect to Docker: %v\n", workerID, err)
		w.markRestartFailed(workerID)
		return
	}
	defer dockerClient.Close()

	// Restart gluetun container first
	fmt.Printf("[%s] Restarting gluetun container '%s'...\n", workerID, gluetunName)
	if err := dockerClient.ContainerRestart(ctx, gluetunName, container.StopOptions{}); err != nil {
		fmt.Printf("[%s] HEAL FAILED: Failed to restart gluetun container: %v\n", workerID, err)
		w.markRestartFailed(workerID)
		return
	}
	fmt.Printf("[%s] Gluetun container '%s' restarted successfully.\n", workerID, gluetunName)

	// Wait for gluetun to initialize VPN connection
	fmt.Printf("[%s] Waiting for VPN to stabilize...\n", workerID)
	time.Sleep(10 * time.Second)

	// Restart cobalt-api container to ensure network namespace sync
	fmt.Printf("[%s] Restarting cobalt-api container '%s' to sync network namespace...\n", workerID, cobaltName)
	if err := dockerClient.ContainerRestart(ctx, cobaltName, container.StopOptions{}); err != nil {
		fmt.Printf("[%s] HEAL FAILED: Failed to restart cobalt-api container: %v\n", workerID, err)
		w.markRestartFailed(workerID)
		return
	}
	fmt.Printf("[%s] Cobalt-api container '%s' restarted successfully.\n", workerID, cobaltName)

	// Wait for cobalt-api to initialize within the network namespace
	fmt.Printf("[%s] Waiting for cobalt-api to stabilize...\n", workerID)
	time.Sleep(5 * time.Second)

	// Health check: verify the worker API is actually reachable
	workerHost := fmt.Sprintf("gluetun-%s", workerID[1:])
	fmt.Printf("[%s] Running health check...\n", workerID)

	if healthCheckWorker(workerHost, 9000) {
		fmt.Printf("[%s] Health check passed.\n", workerID)

		// Cooldown period: 30 seconds without incoming traffic (restarting flag still true)
		fmt.Printf("[%s] COOLDOWN: Waiting 30 seconds before accepting traffic...\n", workerID)
		time.Sleep(30 * time.Second)

		w.mu.Lock()
		for _, worker := range w.workers {
			if worker.ID == workerID {
				worker.Failures = 0
				worker.Healthy = true
				worker.Restarting = false
				worker.RestartScheduled = false
				break
			}
		}
		w.mu.Unlock()
		fmt.Printf("[%s] HEALED: Ready for traffic after cooldown.\n", workerID)
	} else {
		fmt.Printf("[%s] HEAL FAILED: Health check failed after restart\n", workerID)
		w.markRestartFailed(workerID)
	}
}

func (w *Workers) markRestartFailed(workerID string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for _, worker := range w.workers {
		if worker.ID == workerID {
			worker.Healthy = false
			worker.Restarting = false
			worker.RestartScheduled = false
			worker.Failures++
			break
		}
	}
}

// getHealthyWorkers returns a list of healthy workers that are not restarting
func (w *Workers) getHealthyWorkers(exclude map[string]bool) []*Worker {
	w.mu.RLock()
	defer w.mu.RUnlock()

	var healthy []*Worker
	for _, worker := range w.workers {
		if worker.Healthy && !worker.Restarting && !worker.RestartScheduled {
			if exclude == nil || !exclude[worker.ID] {
				healthy = append(healthy, worker)
			}
		}
	}
	return healthy
}

// findWorkerByID finds a worker by ID
func (w *Workers) findWorkerByID(id string) *Worker {
	w.mu.RLock()
	defer w.mu.RUnlock()

	for _, worker := range w.workers {
		if worker.ID == id {
			return worker
		}
	}
	return nil
}

// handleGenerate handles POST / requests (generate link)
func handleGenerate(w http.ResponseWriter, r *http.Request) {
	maxAttempts := 3
	triedWorkers := make(map[string]bool)
	var failedWorkers []string

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		workerSnapshot := workers.getHealthyWorkers(triedWorkers)

		if len(workerSnapshot) == 0 {
			fmt.Println("No healthy workers available!")
			break
		}

		// Random selection
		worker := workerSnapshot[rand.Intn(len(workerSnapshot))]
		triedWorkers[worker.ID] = true

		fmt.Printf("Routing Generate Request to %s (Attempt %d)\n", worker.ID, attempt)

		upstreamURL := fmt.Sprintf("http://%s:%d/", worker.Host, worker.APIPort)

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		req, err := http.NewRequestWithContext(ctx, "POST", upstreamURL, strings.NewReader(string(bodyBytes)))
		if err != nil {
			cancel()
			failedWorkers = append(failedWorkers, worker.ID)
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			cancel()
			fmt.Printf("[%s] Network Error: %v\n", worker.ID, err)
			failedWorkers = append(failedWorkers, worker.ID)
			continue
		}

		status := resp.StatusCode

		if status == http.StatusOK {
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()

			if err != nil {
				fmt.Printf("[%s] Failed to read response body: %v\n", worker.ID, err)
				failedWorkers = append(failedWorkers, worker.ID)
				continue
			}

			// SUCCESS! Schedule restart for all failed workers
			fmt.Printf("Request succeeded on %s after %d failed attempt(s). Scheduling restart for: %v\n",
				worker.ID, len(failedWorkers), failedWorkers)

			for _, failedID := range failedWorkers {
				workers.scheduleRestart(failedID)
			}
			go workers.executeScheduledRestarts()

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}

		// Read response body for error analysis
		responseBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		// Check for critical errors
		isCritical := false
		var errResp ErrorResponse
		if err := json.Unmarshal(responseBody, &errResp); err == nil && errResp.Error != nil {
			code := errResp.Error.Code
			if code == "error.api.fetch.critical" || code == "error.api.fetch.fail" || code == "error.api.youtube.login" {
				isCritical = true
			}
		}

		if status >= 500 || status == http.StatusTooManyRequests || isCritical {
			fmt.Printf("[%s] Failed with %d (Critical: %v). Will retry next worker.\n", worker.ID, status, isCritical)
			failedWorkers = append(failedWorkers, worker.ID)
			continue
		}

		// Regular 4xx — return to user (no restart scheduled)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(status)
		w.Write(responseBody)
		return
	}

	// All attempts failed - NO restart, just return error
	fmt.Printf("All %d attempts failed. Workers tried: %v. No restart triggered.\n", len(triedWorkers), triedWorkers)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(map[string]string{
		"error":  "Service Unavailable",
		"detail": "All instances failed or busy.",
	})
}

// handleTunnel handles GET /tunnel requests (download proxy)
func handleTunnel(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	idParam := query.Get("id")
	serviceParam := query.Get("service")

	if idParam == "" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.Error(w, "Missing ID", http.StatusBadRequest)
		return
	}

	// Extract worker ID prefix (e.g. w1-xxxx -> w1)
	parts := strings.SplitN(idParam, "-", 2)
	workerPrefix := parts[0]

	worker := workers.findWorkerByID(workerPrefix)
	if worker == nil {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.Error(w, "Worker not found for this ID", http.StatusNotFound)
		return
	}

	fmt.Printf("Sticky Route: Tunnel %s -> %s\n", idParam, worker.ID)

	// Build upstream URL preserving original path and query
	upstreamURL := fmt.Sprintf("http://%s:%d%s", worker.Host, worker.APIPort, r.URL.RequestURI())

	// Forward request
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", upstreamURL, nil)
	if err != nil {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.Error(w, "Bad Gateway - Worker might be restarting", http.StatusBadGateway)
		return
	}

	// Forward headers
	for key, values := range r.Header {
		if strings.ToLower(key) == "host" {
			continue
		}
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Printf("[%s] Tunnel Error: %v\n", worker.ID, err)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.Error(w, "Bad Gateway - Worker might be restarting", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	upstreamStatus := resp.StatusCode

	// Check for "Silent Failure" using robust length check
	if upstreamStatus == http.StatusOK {
		estimatedLength := parseLengthHeader(resp.Header.Get("estimated-content-length"))
		realLength := parseLengthHeader(resp.Header.Get("content-length"))

		isSilentFailure := false
		if estimatedLength != nil {
			if *estimatedLength > 0 {
				isSilentFailure = realLength != nil && (*realLength == 0 || *realLength == -1)
			} else if *estimatedLength <= 0 || *estimatedLength == -1 {
				isSilentFailure = realLength == nil || *realLength == 0 || *realLength == -1
			}
		} else {
			isSilentFailure = realLength == nil || *realLength == 0 || *realLength == -1
		}

		if isSilentFailure {
			if serviceParam == "tiktok" {
				fmt.Printf("[%s] Silent Failure Detected but ignored for tiktok.\n", worker.ID)
			} else {
				fmt.Printf("[%s] Silent Failure Detected (Invalid response length). Estimated=%v, Real=%v. Scheduling restart.\n",
					worker.ID, estimatedLength, realLength)

				// Schedule restart for this worker
				go func() {
					workers.scheduleRestart(worker.ID)
					workers.executeScheduledRestarts()
				}()

				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]string{
					"error":  "Stream Blocked",
					"detail": "Origin worker returned invalid content length. Worker is restarting.",
				})
				return
			}
		}
	}

	// Stream response back — pass through headers
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(upstreamStatus)

	// Stream the body
	io.Copy(w, resp.Body)
}

// urlDecodeParams decodes URL query parameters
func urlDecodeParams(query string) url.Values {
	values, _ := url.ParseQuery(query)
	return values
}

// enableCORS enables CORS for all endpoints
func enableCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

func main() {
	mux := http.NewServeMux()

	// POST / — Generate Link
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			handleGenerate(w, r)
		} else if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	})

	// GET /tunnel — Download proxy
	mux.HandleFunc("/tunnel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			handleTunnel(w, r)
		} else if r.Method == "OPTIONS" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	})

	addr := fmt.Sprintf("0.0.0.0:%d", GatewayPort)
	fmt.Printf("Smart Gateway running on port %d\n", GatewayPort)
	fmt.Printf("Managed Workers: %d\n", WorkerCount)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("Server error: %v\n", err)
	}
}
