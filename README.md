# tr143-go

A high-throughput TR-143 test server written in Go, designed to handle sustained 2 Gbps connections on commodity cloud hardware (tested on AWS Lightsail). Used as a backend for TAUC (TP-Link router management platform) QoE speed and latency testing.

## Protocol Overview

Three services run simultaneously:

| Service | Default Port | Protocol | Purpose |
|---|---|---|---|
| HTTP Download | 8080 | TCP / HTTP 1.1 | Streams data to the client for download speed measurement |
| Raw TCP Upload | 8081 | TCP (raw) | Blackhole — reads and discards all incoming bytes, returns HTTP 200 OK when the client finishes |
| UDP Echo | 7 | UDP | Echoes every packet back to the sender for RTT / latency measurement |

All ports are configurable at runtime — see [Configuration](#configuration) below.

## Building

```sh
go build -o tr143-server .
```

Requires Go 1.24+. No external dependencies.

## Running

```sh
# With defaults (ports 8080 / 8081 / 7)
./tr143-server

# UDP echo uses port 7, which is a privileged port (<1024).
# Grant the binary the capability so it can bind without running as root:
sudo setcap 'cap_net_bind_service=+ep' ./tr143-server
./tr143-server

# Or run as root (not recommended in production):
sudo ./tr143-server
```

Startup output confirms active configuration:

```
[*] UDP Echo                     port=7      workers=2
[*] Raw TCP Upload 'Blackhole'   port=8081   buf=1024KB  recvbuf=4MB  idle=10s
[*] HTTP Download Server         port=8080
```

## Configuration

Configuration is loaded in this priority order (highest wins):

1. **Environment variables** — set in the shell or a systemd unit file
2. **`tr143.conf`** — placed in the same directory as the binary
3. **Built-in defaults**

Copy the example file to get started:

```sh
cp tr143.conf.example tr143.conf
```

### All Options

| Key | Default | Description |
|---|---|---|
| `DL_PORT` | `8080` | HTTP download port |
| `UL_PORT` | `8081` | Raw TCP upload port |
| `UDP_PORT` | `7` | UDP echo port (requires `CAP_NET_BIND_SERVICE` or root for ports < 1024) |
| `UL_BUFFER_SIZE` | `1048576` | Upload read buffer in bytes (1 MB). Larger = fewer syscalls at high throughput |
| `TCP_RECV_BUFFER` | `4194304` | Kernel TCP receive socket buffer in bytes (4 MB). Should be ≥ 2× bandwidth-delay product |
| `IDLE_TIMEOUT_SEC` | `10` | Seconds of inactivity before an upload connection is closed |

### Example `tr143.conf`

```ini
DL_PORT  = 8080
UL_PORT  = 8081
UDP_PORT = 7

UL_BUFFER_SIZE   = 1048576  # 1 MB read buffer
TCP_RECV_BUFFER  = 4194304  # 4 MB kernel socket buffer
IDLE_TIMEOUT_SEC = 10
```

### Environment Variable Override

Environment variables are useful for container deployments or one-off overrides without modifying the config file:

```sh
DL_PORT=9090 UL_BUFFER_SIZE=2097152 ./tr143-server
```

## Deployment on AWS Lightsail

### Firewall

Open the following ports in the Lightsail firewall for your instance:

| Port | Protocol |
|---|---|
| 8080 | TCP |
| 8081 | TCP |
| 7 | UDP |

### Running as a systemd Service

Create `/etc/systemd/system/tr143.service`:

```ini
[Unit]
Description=TR-143 Test Server
After=network.target

[Service]
ExecStart=/opt/tr143/tr143-server
WorkingDirectory=/opt/tr143
Restart=always
RestartSec=5
# Allow binding to privileged ports (required for UDP port 7)
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
# Run as an unprivileged user
User=nobody
Group=nogroup

[Install]
WantedBy=multi-user.target
```

Then enable and start:

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now tr143
sudo systemctl status tr143
```

### Kernel TCP Buffer Limits

On Linux, the kernel caps `SO_RCVBUF` at `net.core.rmem_max`. Check and raise it if needed:

```sh
# Check current max
sysctl net.core.rmem_max

# Raise to 8 MB for the current session
sudo sysctl -w net.core.rmem_max=8388608

# Make it permanent
echo 'net.core.rmem_max = 8388608' | sudo tee -a /etc/sysctl.d/99-tr143.conf
sudo sysctl -p /etc/sysctl.d/99-tr143.conf
```

The server's default `TCP_RECV_BUFFER` of 4 MB requires `net.core.rmem_max` to be at least 4194304.

## Performance Notes

The server is tuned for sustained high-throughput single-client tests (one active test at a time), which matches the TAUC usage pattern.

| Optimization | Detail |
|---|---|
| Large upload read buffer | 1 MB default reduces `Read()` syscalls from ~7,800/sec to ~238/sec at 2 Gbps |
| TCP socket buffer (`SO_RCVBUF`) | 4 MB per upload connection prevents kernel-level backpressure |
| Infrequent deadline resets | `SetReadDeadline` called every 4 MB received instead of every read, preserving idle detection with fewer syscalls |
| Pre-allocated download buffer | 1 MB buffer filled once at startup and shared read-only across all download handlers — no per-connection allocation or `/dev/urandom` reads |
| Parallel UDP echo workers | One goroutine per CPU core drains burst probe packets in parallel, avoiding artificial queueing latency |
| Custom HTTP server | `ReadHeaderTimeout` and `IdleTimeout` set; `WriteTimeout` intentionally omitted so download streams are never cut short |
