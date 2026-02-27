use base64::Engine;
use bollard::Docker;
use bytes::Bytes;
use futures_util::StreamExt;
use http_body_util::{combinators::BoxBody, BodyExt, Empty, Full, StreamBody};
use hyper::body::Frame;
use hyper::server::conn::http1;
use hyper::service::service_fn;
use hyper::{Method, Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use rand::seq::SliceRandom;
use reqwest::Client;
use serde::{Deserialize, Serialize};
use std::collections::HashSet;
use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::Arc;
use tokio::net::TcpListener;
use tokio::signal;
use tokio::sync::RwLock;

type BoxError = Box<dyn std::error::Error + Send + Sync>;
type AppBody = BoxBody<Bytes, BoxError>;

// Configuration
const GATEWAY_PORT: u16 = 9111;
const WORKER_COUNT: usize = 5;
const AUTH_USER: &str = "admin";
const AUTH_PASS: &str = "secretpassword";

#[derive(Debug, Clone)]
struct Worker {
    id: String,
    host: String,
    api_port: u16,
    control_port: u16,
    healthy: bool,
    restarting: bool,
    failures: u32,
}

type Workers = Arc<RwLock<Vec<Worker>>>;

fn parse_length_header(value: Option<&str>) -> Option<i64> {
    match value {
        None => None,
        Some(v) => match v.parse::<i64>() {
            Ok(n) => Some(n),
            Err(_) => Some(-1),
        },
    }
}

// Utility: Health check a worker before marking it healthy
async fn health_check_worker(host: &str, port: u16, client: &Client) -> bool {
    let url = format!("http://{}:{}/api/serverInfo", host, port);
    // Try multiple times with longer delay - cobalt API takes time to start
    for attempt in 1..=15 {
        match client
            .get(&url)
            .timeout(std::time::Duration::from_secs(5))
            .send()
            .await
        {
            Ok(res) => {
                if res.status().is_success() || res.status().as_u16() == 404 {
                    // API is up
                    return true;
                }
            }
            Err(e) => {
                println!("[{}] Health check attempt {} failed: {}", host, attempt, e);
            }
        }
        tokio::time::sleep(std::time::Duration::from_secs(3)).await;
    }
    false
}

// Utility: Trigger VPN Restart via Docker (using bollard)
async fn restart_vpn(workers: Workers, worker_id: String, client: Client) {
    // Check if already restarting
    {
        let mut w = workers.write().await;
        if let Some(worker) = w.iter_mut().find(|w| w.id == worker_id) {
            if worker.restarting {
                return;
            }
            worker.restarting = true;
            worker.healthy = false;
        } else {
            return;
        }
    }

    // Map worker_id to container names (w1 -> gluetun-1 & cobalt-api-1)
    let gluetun_name = format!("gluetun-{}", &worker_id[1..]);
    let cobalt_name = format!("cobalt-api-{}", &worker_id[1..]);

    println!("[{}] HEALING: Restarting containers '{}' and '{}'...", worker_id, gluetun_name, cobalt_name);

    let result: Result<(), String> = async {
        // Connect to Docker daemon
        let docker = Docker::connect_with_socket_defaults()
            .map_err(|e| format!("Failed to connect to Docker: {}", e))?;

        // Restart gluetun container first
        println!("[{}] Restarting gluetun container '{}'...", worker_id, gluetun_name);
        docker
            .restart_container(&gluetun_name, None)
            .await
            .map_err(|e| format!("Failed to restart gluetun container: {}", e))?;

        println!("[{}] Gluetun container '{}' restarted successfully.", worker_id, gluetun_name);

        // Wait for gluetun to initialize VPN connection
        println!("[{}] Waiting for VPN to stabilize...", worker_id);
        tokio::time::sleep(std::time::Duration::from_secs(10)).await;

        // Restart cobalt-api container to ensure network namespace sync
        println!("[{}] Restarting cobalt-api container '{}' to sync network namespace...", worker_id, cobalt_name);
        docker
            .restart_container(&cobalt_name, None)
            .await
            .map_err(|e| format!("Failed to restart cobalt-api container: {}", e))?;

        println!("[{}] Cobalt-api container '{}' restarted successfully.", worker_id, cobalt_name);

        // Wait for cobalt-api to initialize within the network namespace
        println!("[{}] Waiting for cobalt-api to stabilize...", worker_id);
        tokio::time::sleep(std::time::Duration::from_secs(5)).await;

        // Health check: verify the worker API is actually reachable
        let worker_host = format!("gluetun-{}", &worker_id[1..]);
        println!("[{}] Running health check...", worker_id);

        if health_check_worker(&worker_host, 9000, &client).await {
            println!("[{}] Health check passed.", worker_id);
            Ok(())
        } else {
            Err("Health check failed after restart".to_string())
        }
    }
    .await;

    let mut w = workers.write().await;
    if let Some(worker) = w.iter_mut().find(|w| w.id == worker_id) {
        match result {
            Ok(()) => {
                worker.failures = 0;
                worker.healthy = true;
                worker.restarting = false;
                println!("[{}] HEALED: Ready for traffic.", worker_id);
            }
            Err(err) => {
                eprintln!("[{}] HEAL FAILED: {}", worker_id, err);
                // Keep unhealthy if heal failed, don't immediately retry
                worker.healthy = false;
                worker.restarting = false;
                worker.failures += 1;
            }
        }
    }
}

#[derive(Deserialize, Serialize, Debug)]
struct ErrorResponse {
    error: Option<ErrorDetail>,
}

#[derive(Deserialize, Serialize, Debug)]
struct ErrorDetail {
    code: Option<String>,
}

// Handler: POST / (Generate Link)
async fn handle_generate(
    body_bytes: Bytes,
    workers: Workers,
    client: Client,
) -> Response<AppBody> {
    let max_attempts = 3;
    let mut tried_workers = HashSet::new();

    for attempt in 1..=max_attempts {
        // Pick a healthy worker
        let worker_snapshot = {
            let w = workers.read().await;
            let healthy: Vec<Worker> = w
                .iter()
                .filter(|w| w.healthy && !tried_workers.contains(&w.id))
                .cloned()
                .collect();
            healthy
        };

        if worker_snapshot.is_empty() {
            eprintln!("No healthy workers available!");
            break;
        }

        let worker = worker_snapshot
            .choose(&mut rand::thread_rng())
            .unwrap()
            .clone();
        tried_workers.insert(worker.id.clone());

        println!(
            "Routing Generate Request to {} (Attempt {})",
            worker.id, attempt
        );

        let url = format!("http://{}:{}/", worker.host, worker.api_port);

        match client
            .post(&url)
            .header("Content-Type", "application/json")
            .header("Accept", "application/json")
            .timeout(std::time::Duration::from_secs(30))
            .body(body_bytes.clone())
            .send()
            .await
        {
            Ok(result) => {
                let status = result.status();

                if status == StatusCode::OK {
                    match result.bytes().await {
                        Ok(data) => {
                            return Response::builder()
                                .status(200)
                                .header("Content-Type", "application/json")
                                .header("Access-Control-Allow-Origin", "*")
                                .body(full_body(data))
                                .unwrap();
                        }
                        Err(e) => {
                            eprintln!("[{}] Failed to read response body: {}", worker.id, e);
                            let wc = workers.clone();
                            let cc = client.clone();
                            let wid = worker.id.clone();
                            tokio::spawn(async move { restart_vpn(wc, wid, cc).await });
                            continue;
                        }
                    }
                } else {
                    let response_body = result.text().await.unwrap_or_default();

                    // Check for critical errors
                    let mut is_critical = false;
                    if let Ok(json_body) = serde_json::from_str::<ErrorResponse>(&response_body) {
                        if let Some(error) = json_body.error {
                            if let Some(code) = error.code {
                                if code == "error.api.fetch.critical"
                                    || code == "error.api.fetch.fail"
                                    || code == "error.api.youtube.login"
                                {
                                    is_critical = true;
                                }
                            }
                        }
                    }

                    if status.as_u16() >= 500 || status == StatusCode::TOO_MANY_REQUESTS || is_critical
                    {
                        eprintln!(
                            "[{}] Failed with {} (Critical: {}). Triggering heal.",
                            worker.id,
                            status.as_u16(),
                            is_critical
                        );
                        let wc = workers.clone();
                        let cc = client.clone();
                        let wid = worker.id.clone();
                        tokio::spawn(async move { restart_vpn(wc, wid, cc).await });
                        continue;
                    }

                    // Regular 4xx — return to user
                    return Response::builder()
                        .status(status)
                        .header("Content-Type", "application/json")
                        .header("Access-Control-Allow-Origin", "*")
                        .body(full_body(Bytes::from(response_body)))
                        .unwrap();
                }
            }
            Err(err) => {
                eprintln!("[{}] Network Error: {}", worker.id, err);
                let wc = workers.clone();
                let cc = client.clone();
                let wid = worker.id.clone();
                tokio::spawn(async move { restart_vpn(wc, wid, cc).await });
                continue;
            }
        }
    }

    // All attempts failed
    let body = serde_json::json!({
        "error": "Service Unavailable",
        "detail": "All instances failed or busy."
    });
    Response::builder()
        .status(503)
        .header("Content-Type", "application/json")
        .header("Access-Control-Allow-Origin", "*")
        .body(full_body(Bytes::from(body.to_string())))
        .unwrap()
}

// Handler: GET /tunnel (Download — streaming proxy)
async fn handle_tunnel(
    req: Request<hyper::body::Incoming>,
    workers: Workers,
    client: Client,
) -> Response<AppBody> {
    let uri = req.uri();
    let query = uri.query().unwrap_or("");

    // Parse query params
    let params: Vec<(String, String)> = url_decode_params(query);
    let id_param = params.iter().find(|(k, _)| k == "id").map(|(_, v)| v.as_str());
    let service_param = params
        .iter()
        .find(|(k, _)| k == "service")
        .map(|(_, v)| v.as_str());

    let id_param = match id_param {
        Some(id) => id,
        None => {
            return Response::builder()
                .status(400)
                .header("Access-Control-Allow-Origin", "*")
                .body(full_body(Bytes::from("Missing ID")))
                .unwrap();
        }
    };

    // Extract worker ID prefix (e.g. w1-xxxx -> w1)
    let worker_prefix = id_param.split('-').next().unwrap_or("");

    let worker_snapshot = {
        let w = workers.read().await;
        w.iter().find(|w| w.id == worker_prefix).cloned()
    };

    let worker = match worker_snapshot {
        Some(w) => w,
        None => {
            return Response::builder()
                .status(404)
                .header("Access-Control-Allow-Origin", "*")
                .body(full_body(Bytes::from("Worker not found for this ID")))
                .unwrap();
        }
    };

    println!("Sticky Route: Tunnel {} -> {}", id_param, worker.id);

    // Build upstream URL preserving original path and query
    let upstream_url = format!(
        "http://{}:{}{}",
        worker.host,
        worker.api_port,
        req.uri().path_and_query().map(|pq| pq.as_str()).unwrap_or("/")
    );

    // Forward headers
    let mut upstream_req = client.get(&upstream_url);
    for (key, value) in req.headers() {
        if key == "host" {
            continue;
        }
        if let Ok(v) = value.to_str() {
            upstream_req = upstream_req.header(key.as_str(), v);
        }
    }

    match upstream_req.send().await {
        Ok(upstream_res) => {
            let upstream_status = upstream_res.status();

            // Check for "Silent Failure" using robust length check
            if upstream_status == StatusCode::OK {
                let estimated_length = parse_length_header(
                    upstream_res
                        .headers()
                        .get("estimated-content-length")
                        .and_then(|v| v.to_str().ok()),
                );
                let real_length = parse_length_header(
                    upstream_res
                        .headers()
                        .get("content-length")
                        .and_then(|v| v.to_str().ok()),
                );

                let is_silent_failure = match (estimated_length, real_length) {
                    (Some(est), _) if est > 0 => {
                        matches!(real_length, Some(0) | Some(-1))
                    }
                    (Some(est), _) if est <= 0 || est == -1 => {
                        matches!(real_length, Some(0) | None | Some(-1))
                    }
                    (None, _) => {
                        matches!(real_length, Some(0) | None | Some(-1))
                    }
                    _ => false,
                };

                if is_silent_failure {
                    if service_param == Some("tiktok") {
                        println!(
                            "[{}] Silent Failure Detected but ignored for tiktok.",
                            worker.id
                        );
                    } else {
                        eprintln!(
                            "[{}] Silent Failure Detected (Invalid response length). Estimated={:?}, Real={:?}. Triggering heal.",
                            worker.id, estimated_length, real_length
                        );
                        let wc = workers.clone();
                        let cc = client.clone();
                        let wid = worker.id.clone();
                        tokio::spawn(async move { restart_vpn(wc, wid, cc).await });

                        let body = serde_json::json!({
                            "error": "Stream Blocked",
                            "detail": "Origin worker returned invalid content length. Worker is restarting."
                        });
                        return Response::builder()
                            .status(502)
                            .header("Content-Type", "application/json")
                            .header("Access-Control-Allow-Origin", "*")
                            .body(full_body(Bytes::from(body.to_string())))
                            .unwrap();
                    }
                }
            }

            // Stream response back — pass through headers
            let mut response_builder = Response::builder().status(upstream_status.as_u16());

            for (key, value) in upstream_res.headers() {
                response_builder = response_builder.header(key.as_str(), value.as_bytes());
            }
            response_builder =
                response_builder.header("Access-Control-Allow-Origin", "*");

            // Stream the body
            let stream = upstream_res.bytes_stream().map(|result| {
                result
                    .map(Frame::data)
                    .map_err(|e| -> BoxError {
                        eprintln!("Stream error: {}", e);
                        Box::new(e)
                    })
            });

            let body = StreamBody::new(stream);
            response_builder.body(BodyExt::boxed(body)).unwrap()
        }
        Err(err) => {
            eprintln!("[{}] Tunnel Error: {}", worker.id, err);
            Response::builder()
                .status(502)
                .header("Access-Control-Allow-Origin", "*")
                .body(full_body(Bytes::from(
                    "Bad Gateway - Worker might be restarting",
                )))
                .unwrap()
        }
    }
}

fn url_decode_params(query: &str) -> Vec<(String, String)> {
    query
        .split('&')
        .filter(|s| !s.is_empty())
        .filter_map(|pair| {
            let mut parts = pair.splitn(2, '=');
            let key = parts.next()?;
            let value = parts.next().unwrap_or("");
            Some((
                urlencoding_decode(key),
                urlencoding_decode(value),
            ))
        })
        .collect()
}

fn urlencoding_decode(s: &str) -> String {
    let mut result = String::with_capacity(s.len());
    let mut chars = s.bytes();
    while let Some(b) = chars.next() {
        if b == b'%' {
            let h = chars.next().unwrap_or(b'0');
            let l = chars.next().unwrap_or(b'0');
            let byte = hex_val(h) * 16 + hex_val(l);
            result.push(byte as char);
        } else if b == b'+' {
            result.push(' ');
        } else {
            result.push(b as char);
        }
    }
    result
}

fn hex_val(b: u8) -> u8 {
    match b {
        b'0'..=b'9' => b - b'0',
        b'a'..=b'f' => b - b'a' + 10,
        b'A'..=b'F' => b - b'A' + 10,
        _ => 0,
    }
}

fn full_body(data: Bytes) -> AppBody {
    Full::new(data)
        .map_err(|never| -> BoxError { match never {} })
        .boxed()
}

fn empty_body() -> AppBody {
    Empty::<Bytes>::new()
        .map_err(|never| -> BoxError { match never {} })
        .boxed()
}

async fn handle_request(
    req: Request<hyper::body::Incoming>,
    workers: Workers,
    client: Client,
) -> Result<Response<AppBody>, Infallible> {
    let method = req.method().clone();
    let path = req.uri().path().to_string();

    // CORS Preflight
    if method == Method::OPTIONS {
        return Ok(Response::builder()
            .status(204)
            .header("Access-Control-Allow-Origin", "*")
            .header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
            .header("Access-Control-Allow-Headers", "Content-Type, Accept")
            .body(empty_body())
            .unwrap());
    }

    // POST / — Generate Link
    if method == Method::POST && path == "/" {
        let body_bytes = match req.collect().await {
            Ok(collected) => collected.to_bytes(),
            Err(e) => {
                eprintln!("Failed to read request body: {}", e);
                return Ok(Response::builder()
                    .status(400)
                    .header("Access-Control-Allow-Origin", "*")
                    .body(full_body(Bytes::from("Bad Request")))
                    .unwrap());
            }
        };
        return Ok(handle_generate(body_bytes, workers, client).await);
    }

    // GET /tunnel — Download proxy
    if method == Method::GET && path.starts_with("/tunnel") {
        return Ok(handle_tunnel(req, workers, client).await);
    }

    // 404
    Ok(Response::builder()
        .status(404)
        .header("Access-Control-Allow-Origin", "*")
        .body(full_body(Bytes::from("Not Found")))
        .unwrap())
}

#[tokio::main]
async fn main() {
    // Initialize workers
    let mut worker_list = Vec::with_capacity(WORKER_COUNT);
    for i in 1..=WORKER_COUNT {
        worker_list.push(Worker {
            id: format!("w{}", i),
            host: format!("gluetun-{}", i),
            api_port: 9000,
            control_port: 8000,
            healthy: true,
            restarting: false,
            failures: 0,
        });
    }
    println!("Initialized {} workers.", worker_list.len());

    let workers: Workers = Arc::new(RwLock::new(worker_list));

    // Shared HTTP client with proper timeout settings
    let client = Client::builder()
        .pool_max_idle_per_host(10)
        .connect_timeout(std::time::Duration::from_secs(10))
        .timeout(std::time::Duration::from_secs(60))  // Global timeout fallback
        .build()
        .expect("Failed to build HTTP client");

    let addr = SocketAddr::from(([0, 0, 0, 0], GATEWAY_PORT));
    let listener = TcpListener::bind(addr)
        .await
        .expect("Failed to bind address");

    println!("Smart Gateway running on port {}", GATEWAY_PORT);
    println!("Managed Workers: {}", WORKER_COUNT);

    // Graceful shutdown signal
    let shutdown = async {
        let ctrl_c = signal::ctrl_c();
        let mut sigterm = signal::unix::signal(signal::unix::SignalKind::terminate())
            .expect("Failed to install SIGTERM handler");

        tokio::select! {
            _ = ctrl_c => println!("SIGINT received, shutting down gracefully..."),
            _ = sigterm.recv() => println!("SIGTERM received, shutting down gracefully..."),
        }
    };

    // Serve connections
    let server = async {
        loop {
            let (stream, _remote_addr) = match listener.accept().await {
                Ok(conn) => conn,
                Err(e) => {
                    eprintln!("Accept error: {}", e);
                    continue;
                }
            };

            let io = TokioIo::new(stream);
            let workers = workers.clone();
            let client = client.clone();

            tokio::spawn(async move {
                let service = service_fn(move |req| {
                    let workers = workers.clone();
                    let client = client.clone();
                    async move { handle_request(req, workers, client).await }
                });

                if let Err(err) = http1::Builder::new()
                    .serve_connection(io, service)
                    .with_upgrades()
                    .await
                {
                    // Filter out benign connection reset errors
                    let err_str = err.to_string();
                    if !err_str.contains("connection reset")
                        && !err_str.contains("broken pipe")
                        && !err_str.contains("incomplete message")
                    {
                        eprintln!("Connection error: {}", err);
                    }
                }
            });
        }
    };

    tokio::select! {
        _ = server => {},
        _ = shutdown => {
            println!("Server closed");
        },
    }
}
