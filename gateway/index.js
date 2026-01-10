import http from 'http';
import { request } from 'undici';

// Configuration
const GATEWAY_PORT = 9111;
const WORKER_COUNT = 5;
const AUTH_USER = "admin";
const AUTH_PASS = "secretpassword";

// Initialize Workers
const workers = [];
for (let i = 1; i <= WORKER_COUNT; i++) {
    workers.push({
        id: `w${i}`,
        host: `gluetun-${i}`, // Use gluetun hostname because cobalt shares network
        apiPort: 9000,
        controlPort: 8000,
        healthy: true,
        restarting: false,
        failures: 0
    });
}

console.log(`Initialized ${workers.length} workers.`);

function parseLengthHeader(value) {
    if (value === undefined) {
        return null;
    }
    const parsed = Number.parseInt(value, 10);
    return Number.isNaN(parsed) ? -1 : parsed;
}

// Utility: Trigger VPN Restart
async function restartVPN(worker) {
    if (worker.restarting) return;
    worker.restarting = true;
    worker.healthy = false;
    console.log(`[${worker.id}] HEALING: Triggering VPN Restart...`);

    const authHeader = 'Basic ' + Buffer.from(`${AUTH_USER}:${AUTH_PASS}`).toString('base64');
    const controlUrl = `http://${worker.host}:${worker.controlPort}/v1/vpn/status`;

    try {
        // Stop VPN
        await request(controlUrl, {
            method: 'PUT',
            headers: { 'Authorization': authHeader, 'Content-Type': 'application/json' },
            body: JSON.stringify({ status: "stopped" })
        });
        console.log(`[${worker.id}] VPN Stopped.`);

        // Wait 2 seconds
        await new Promise(r => setTimeout(r, 2000));

        // Start VPN
        await request(controlUrl, {
            method: 'PUT',
            headers: { 'Authorization': authHeader, 'Content-Type': 'application/json' },
            body: JSON.stringify({ status: "running" })
        });
        console.log(`[${worker.id}] VPN Started.`);

        // Wait for connection to stabilize (10s)
        await new Promise(r => setTimeout(r, 10000));

        // Reset Health
        worker.failures = 0;
        worker.healthy = true;
        worker.restarting = false;
        console.log(`[${worker.id}] HEALED: Ready for traffic.`);
    } catch (err) {
        console.error(`[${worker.id}] HEAL FAILED: ${err.message}`);
        // For this implementation, let's reset to healthy to give it another chance.
        worker.healthy = true;
    }
}

// Handler: POST / (Generate Link)
async function handleGenerate(req, res, bodyBuffer) {
    let attempts = 0;
    // Try up to 3 different workers
    const maxAttempts = 3;
    const triedWorkers = new Set();

    while (attempts < maxAttempts) {
        attempts++;

        // Pick a healthy worker (Round Robin or Random from healthy pool)
        const healthyWorkers = workers.filter(w => w.healthy && !triedWorkers.has(w.id));

        if (healthyWorkers.length === 0) {
            console.error("No healthy workers available!");
            break;
        }

        // Random pick for better distribution in this simple implementation
        const worker = healthyWorkers[Math.floor(Math.random() * healthyWorkers.length)];
        triedWorkers.add(worker.id);

        console.log(`Routing Generate Request to ${worker.id} (Attempt ${attempts})`);

        try {
            const result = await request(`http://${worker.host}:${worker.apiPort}/`, {
                method: 'POST',
                headers: {
                    'Content-Type': 'application/json',
                    'Accept': 'application/json'
                },
                body: bodyBuffer
            });

            // If success
            if (result.statusCode === 200) {
                const data = await result.body.json();
                res.writeHead(200, { 'Content-Type': 'application/json' });
                res.end(JSON.stringify(data));
                return;
            } else {
                // If 4xx/5xx (likely blocked or rate limited)
                const responseBody = await result.body.text();

                // Check for critical blocking error 'error.api.fetch.critical', etc
                let isCriticalError = false;
                try {
                    const jsonBody = JSON.parse(responseBody);
                    if (jsonBody.error && (
                        jsonBody.error.code === 'error.api.fetch.critical' ||
                        jsonBody.error.code === 'error.api.fetch.fail' ||
                        jsonBody.error.code === 'error.api.youtube.login'
                    )) {
                        isCriticalError = true;
                    }
                } catch (e) { }

                if (result.statusCode >= 500 || result.statusCode === 429 || isCriticalError) {
                    console.warn(`[${worker.id}] Failed with ${result.statusCode} (Critical: ${isCriticalError}). Triggering heal.`);
                    restartVPN(worker); // Fire and forget
                    continue; // Loop to next attempt
                }

                // If regular 400 or other 4xx, return to user
                res.writeHead(result.statusCode, { 'Content-Type': 'application/json' });
                res.end(responseBody);
                return;
            }
        } catch (err) {
            console.error(`[${worker.id}] Network Error: ${err.message}`);
            restartVPN(worker);
            continue;
        }
    }

    // If all attempts fail
    res.writeHead(503, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ error: "Service Unavailable", detail: "All instances failed or busy." }));
}

// Handler: GET /tunnel (Download)
async function handleTunnel(req, res, url) {
    const idParam = url.searchParams.get('id');

    if (!idParam) {
        res.writeHead(400);
        res.end("Missing ID");
        return;
    }

    // Extract worker ID (e.g. w1-xxxx -> w1)
    const workerPrefix = idParam.split('-')[0];
    const worker = workers.find(w => w.id === workerPrefix);

    if (!worker) {
        res.writeHead(404);
        res.end("Worker not found for this ID");
        return;
    }

    // Sticky Route
    console.log(`Sticky Route: Tunnel ${idParam} -> ${worker.id}`);

    try {
        // Stream the response back
        // Using http.request node native for piping streams easily
        const proxyReq = http.request({
            hostname: worker.host,
            port: worker.apiPort,
            path: req.url,
            method: req.method,
            headers: req.headers
        }, (proxyRes) => {
            // Check for "Silent Failure" using robust length check
            const estimatedLength = parseLengthHeader(proxyRes.headers['estimated-content-length']);
            const realLength = parseLengthHeader(proxyRes.headers['content-length']);

            if (proxyRes.statusCode === 200) {
                if (
                    (estimatedLength > 0 && (realLength === 0 || realLength === -1)) ||
                    ((estimatedLength <= 0 || estimatedLength === -1) && (realLength === 0 || realLength === null || realLength === -1))
                ) {
                    console.warn(`[${worker.id}] Silent Failure Detected (Invalid response length). Estimated=${estimatedLength}, Real=${realLength}. Triggering heal.`);
                    restartVPN(worker);

                    res.writeHead(502, { 'Content-Type': 'application/json' });
                    res.end(JSON.stringify({
                        error: "Stream Blocked",
                        detail: "Origin worker returned invalid content length. Worker is restarting."
                    }));
                    return;
                }
            }

            // Normal pass-through
            res.writeHead(proxyRes.statusCode, proxyRes.headers);
            proxyRes.pipe(res);
        });

        proxyReq.on('error', (err) => {
            console.error(`[${worker.id}] Tunnel Error: ${err.message}`);
            // If connection refused, maybe restarting?
            if (!res.headersSent) {
                res.writeHead(502);
                res.end("Bad Gateway - Worker might be restarting");
            }
        });

        req.pipe(proxyReq);
    } catch (err) {
        res.writeHead(500);
        res.end("Internal Gateway Error");
    }
}

// Server
const server = http.createServer(async (req, res) => {
    // Collect body for POST
    const chunks = [];

    req.on('data', chunk => chunks.push(chunk));
    req.on('end', async () => {
        const bodyBuffer = Buffer.concat(chunks);
        const url = new URL(req.url, `http://${req.headers.host}`);

        // CORS / Options
        if (req.method === 'OPTIONS') {
            res.writeHead(204, {
                'Access-Control-Allow-Origin': '*',
                'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
                'Access-Control-Allow-Headers': 'Content-Type, Accept'
            });
            res.end();
            return;
        }

        // Add standard CORS headers to all responses
        res.setHeader('Access-Control-Allow-Origin', '*');

        if (req.method === 'POST' && url.pathname === '/') {
            await handleGenerate(req, res, bodyBuffer);
        } else if (req.method === 'GET' && url.pathname.startsWith('/tunnel')) {
            await handleTunnel(req, res, url);
        } else {
            res.writeHead(404);
            res.end("Not Found");
        }
    });
});

server.listen(GATEWAY_PORT, () => {
    console.log(`Smart Gateway running on port ${GATEWAY_PORT}`);
    console.log(`Managed Workers: ${WORKER_COUNT}`);
});
