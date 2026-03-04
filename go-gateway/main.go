package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
		Timeout: 0, // No global timeout, use context for per-request control
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
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
			time.Sleep(3 * time.Second)
			continue
		}

		resp, err := httpClient.Do(req)
		cancel()

		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNotFound {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	fmt.Printf("[%s] Health check failed after 15 attempts\n", host)
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
	w.mu.Lock()
	var worker *Worker
	for _, wkr := range w.workers {
		if wkr.ID == workerID {
			worker = wkr
			break
		}
	}
	if worker == nil || worker.Restarting {
		w.mu.Unlock()
		return
	}
	worker.Restarting = true
	worker.Healthy = false
	worker.RestartScheduled = false
	w.mu.Unlock()

	gluetunName := fmt.Sprintf("gluetun-%s", workerID[1:])
	cobaltName := fmt.Sprintf("cobalt-api-%s", workerID[1:])

	fmt.Printf("[%s] Restarting containers...\n", workerID)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Printf("[%s] Docker connect failed: %v\n", workerID, err)
		w.markRestartFailed(workerID)
		return
	}
	defer dockerClient.Close()

	if err := dockerClient.ContainerRestart(ctx, gluetunName, container.StopOptions{}); err != nil {
		fmt.Printf("[%s] Gluetun restart failed: %v\n", workerID, err)
		w.markRestartFailed(workerID)
		return
	}
	time.Sleep(10 * time.Second)

	if err := dockerClient.ContainerRestart(ctx, cobaltName, container.StopOptions{}); err != nil {
		fmt.Printf("[%s] Cobalt restart failed: %v\n", workerID, err)
		w.markRestartFailed(workerID)
		return
	}
	time.Sleep(5 * time.Second)

	workerHost := fmt.Sprintf("gluetun-%s", workerID[1:])
	if healthCheckWorker(workerHost, 9000) {
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
		fmt.Printf("[%s] Healed\n", workerID)
	} else {
		fmt.Printf("[%s] Health check failed after restart\n", workerID)
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

	// Limit request body size to prevent OOM
	r.Body = http.MaxBytesReader(w, r.Body, 10*1024*1024) // 10MB limit
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
			failedWorkers = append(failedWorkers, worker.ID)
			continue
		}

		status := resp.StatusCode

		if status == http.StatusOK {
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()

			if err != nil {
				failedWorkers = append(failedWorkers, worker.ID)
				continue
			}

			if len(failedWorkers) > 0 {
				fmt.Printf("[%s] Success after %d failures, restarting: %v\n", worker.ID, len(failedWorkers), failedWorkers)
				for _, failedID := range failedWorkers {
					workers.scheduleRestart(failedID)
				}
				go workers.executeScheduledRestarts()
			}

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}

		responseBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()

		isCritical := false
		var errResp ErrorResponse
		if err := json.Unmarshal(responseBody, &errResp); err == nil && errResp.Error != nil {
			code := errResp.Error.Code
			fmt.Printf("[%s] Cobalt error: %s\n", worker.ID, code)
			if code == "error.api.fetch.critical" || code == "error.api.fetch.fail" || code == "error.api.youtube.login" {
				isCritical = true
			}
		} else if status >= 400 {
			fmt.Printf("[%s] Cobalt HTTP error: %d, body: %s\n", worker.ID, status, string(responseBody))
		}

		if status >= 500 || status == http.StatusTooManyRequests || isCritical {
			failedWorkers = append(failedWorkers, worker.ID)
			continue
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(status)
		w.Write(responseBody)
		return
	}

	fmt.Printf("All attempts failed: %v\n", triedWorkers)
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

	upstreamURL := fmt.Sprintf("http://%s:%d%s", worker.Host, worker.APIPort, r.URL.RequestURI())

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", upstreamURL, nil)
	if err != nil {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}

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
		w.Header().Set("Access-Control-Allow-Origin", "*")
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	upstreamStatus := resp.StatusCode

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

		if isSilentFailure && serviceParam != "tiktok" {
			go func() {
				workers.scheduleRestart(worker.ID)
				workers.executeScheduledRestarts()
			}()

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{
				"error":  "Stream Blocked",
				"detail": "Invalid content length",
			})
			return
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

	// Flush headers immediately for streaming
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// Stream the body with client disconnect detection
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// Handle panic from writing to closed/invalid ResponseWriter
				done <- fmt.Errorf("stream panic: %v", r)
			}
		}()
		_, err := io.Copy(w, resp.Body)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			fmt.Printf("[%s] Stream error: %v\n", workerPrefix, err)
		}
		// Normal completion or error
	case <-r.Context().Done():
		resp.Body.Close()
	}
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

	// Setup graceful shutdown
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Server error: %v\n", err)
		}
	}()

	<-ctx.Done()
	fmt.Println("\nShutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		fmt.Printf("Shutdown error: %v\n", err)
	}
	fmt.Println("Server stopped.")
}
