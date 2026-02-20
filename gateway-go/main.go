package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	gatewayPort = 9111
	workerCount = 5
	authUser    = "admin"
	authPass    = "secretpassword"
	apiPort     = 9000
	controlPort = 8000
)

type Worker struct {
	mu         sync.Mutex
	id         string
	host       string
	apiPort    int
	controlPort int
	healthy    bool
	restarting bool
	failures   int
}

var workers []*Worker

func init() {
	for i := 1; i <= workerCount; i++ {
		workers = append(workers, &Worker{
			id:          fmt.Sprintf("w%d", i),
			host:        fmt.Sprintf("gluetun-%d", i),
			apiPort:     apiPort,
			controlPort: controlPort,
			healthy:     true,
			restarting:  false,
			failures:    0,
		})
	}
	log.Printf("Initialized %d workers.", len(workers))
}

func parseLengthHeader(value string) int64 {
	if value == "" {
		return -1
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return -1
	}
	return parsed
}

func basicAuth() string {
	creds := authUser + ":" + authPass
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
}

func restartVPN(worker *Worker) {
	worker.mu.Lock()
	if worker.restarting {
		worker.mu.Unlock()
		return
	}
	worker.restarting = true
	worker.healthy = false
	worker.mu.Unlock()

	log.Printf("[%s] HEALING: Triggering VPN Restart...", worker.id)

	controlURL := fmt.Sprintf("http://%s:%d/v1/vpn/status", worker.host, worker.controlPort)
	client := &http.Client{Timeout: 15 * time.Second}

	doRequest := func(status string) error {
		body, _ := json.Marshal(map[string]string{"status": status})
		req, err := http.NewRequest(http.MethodPut, controlURL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", basicAuth())
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return nil
	}

	if err := doRequest("stopped"); err != nil {
		log.Printf("[%s] HEAL FAILED (stop): %v", worker.id, err)
		worker.mu.Lock()
		worker.healthy = true
		worker.restarting = false
		worker.mu.Unlock()
		return
	}
	log.Printf("[%s] VPN Stopped.", worker.id)

	time.Sleep(2 * time.Second)

	if err := doRequest("running"); err != nil {
		log.Printf("[%s] HEAL FAILED (start): %v", worker.id, err)
		worker.mu.Lock()
		worker.healthy = true
		worker.restarting = false
		worker.mu.Unlock()
		return
	}
	log.Printf("[%s] VPN Started.", worker.id)

	time.Sleep(10 * time.Second)

	worker.mu.Lock()
	worker.failures = 0
	worker.healthy = true
	worker.restarting = false
	worker.mu.Unlock()
	log.Printf("[%s] HEALED: Ready for traffic.", worker.id)
}

func pickHealthyWorker(tried map[string]bool) *Worker {
	var pool []*Worker
	for _, w := range workers {
		w.mu.Lock()
		ok := w.healthy && !tried[w.id]
		w.mu.Unlock()
		if ok {
			pool = append(pool, w)
		}
	}
	if len(pool) == 0 {
		return nil
	}
	return pool[rand.Intn(len(pool))]
}

func handleGenerate(w http.ResponseWriter, r *http.Request, body []byte) {
	const maxAttempts = 3
	tried := make(map[string]bool)
	client := &http.Client{Timeout: 30 * time.Second}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		worker := pickHealthyWorker(tried)
		if worker == nil {
			log.Println("No healthy workers available!")
			break
		}
		tried[worker.id] = true
		log.Printf("Routing Generate Request to %s (Attempt %d)", worker.id, attempt)

		targetURL := fmt.Sprintf("http://%s:%d/", worker.host, worker.apiPort)
		req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
		if err != nil {
			log.Printf("[%s] Request build error: %v", worker.id, err)
			go restartVPN(worker)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[%s] Network Error: %v", worker.id, err)
			go restartVPN(worker)
			continue
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(respBody)
			return
		}

		isCritical := false
		var jsonBody map[string]interface{}
		if json.Unmarshal(respBody, &jsonBody) == nil {
			if errObj, ok := jsonBody["error"].(map[string]interface{}); ok {
				code, _ := errObj["code"].(string)
				switch code {
				case "error.api.fetch.critical", "error.api.fetch.fail", "error.api.youtube.login":
					isCritical = true
				}
			}
		}

		if resp.StatusCode >= 500 || resp.StatusCode == 429 || isCritical {
			log.Printf("[%s] Failed with %d (Critical: %v). Triggering heal.", worker.id, resp.StatusCode, isCritical)
			go restartVPN(worker)
			continue
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(respBody)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(map[string]string{
		"error":  "Service Unavailable",
		"detail": "All instances failed or busy.",
	})
}

func handleTunnel(w http.ResponseWriter, r *http.Request, u *url.URL) {
	idParam := u.Query().Get("id")
	serviceParam := u.Query().Get("service")

	if idParam == "" {
		http.Error(w, "Missing ID", http.StatusBadRequest)
		return
	}

	workerPrefix := strings.SplitN(idParam, "-", 2)[0]
	var worker *Worker
	for _, wk := range workers {
		if wk.id == workerPrefix {
			worker = wk
			break
		}
	}

	if worker == nil {
		http.Error(w, "Worker not found for this ID", http.StatusNotFound)
		return
	}

	log.Printf("Sticky Route: Tunnel %s -> %s", idParam, worker.id)

	targetURL := fmt.Sprintf("http://%s:%d%s", worker.host, worker.apiPort, r.RequestURI)
	proxyReq, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "Internal Gateway Error", http.StatusInternalServerError)
		return
	}
	proxyReq.Header = r.Header.Clone()

	client := &http.Client{
		Timeout: 0, // No timeout for streaming
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(proxyReq)
	if err != nil {
		log.Printf("[%s] Tunnel Error: %v", worker.id, err)
		http.Error(w, "Bad Gateway - Worker might be restarting", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		estimatedLength := parseLengthHeader(resp.Header.Get("estimated-content-length"))
		realLength := parseLengthHeader(resp.Header.Get("content-length"))

		silentFailure := (estimatedLength > 0 && (realLength == 0 || realLength == -1)) ||
			((estimatedLength <= 0 || estimatedLength == -1) && (realLength == 0 || realLength == -1))

		if silentFailure {
			if serviceParam == "tiktok" {
				log.Printf("[%s] Silent Failure Detected but ignored for %s.", worker.id, serviceParam)
			} else {
				log.Printf("[%s] Silent Failure Detected (Invalid response length). Estimated=%d, Real=%d. Triggering heal.", worker.id, estimatedLength, realLength)
				go restartVPN(worker)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				json.NewEncoder(w).Encode(map[string]string{
					"error":  "Stream Blocked",
					"detail": "Origin worker returned invalid content length. Worker is restarting.",
				})
				return
			}
		}
	}

	for key, vals := range resp.Header {
		for _, val := range vals {
			w.Header().Add(key, val)
		}
	}
	w.WriteHeader(resp.StatusCode)

	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(w, resp.Body)
	}()

	select {
	case <-r.Context().Done():
		log.Printf("[%s] Client disconnected, aborting tunnel.", worker.id)
	case <-done:
	}
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next(w, r)
	}
}

func router(w http.ResponseWriter, r *http.Request) {
	u, err := url.ParseRequestURI(r.RequestURI)
	if err != nil {
		http.Error(w, "Invalid URL", http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodPost && u.Path == "/" {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		handleGenerate(w, r, body)
	} else if r.Method == http.MethodGet && strings.HasPrefix(u.Path, "/tunnel") {
		handleTunnel(w, r, u)
	} else {
		http.Error(w, "Not Found", http.StatusNotFound)
	}
}

func main() {
	addr := fmt.Sprintf(":%d", gatewayPort)
	srv := &http.Server{
		Addr:    addr,
		Handler: http.HandlerFunc(corsMiddleware(router)),
	}

	go func() {
		log.Printf("Smart Gateway running on port %d", gatewayPort)
		log.Printf("Managed Workers: %d", workerCount)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	<-quit

	log.Println("Shutting down gracefully...")
	srv.Close()
	log.Println("Server closed")
}
