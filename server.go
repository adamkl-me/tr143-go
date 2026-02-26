package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------
// CONFIGURATION
// Loaded from tr143.conf in the same directory as the executable,
// with environment variables taking precedence over file values.
// See tr143.conf.example for documentation.
// ---------------------------------------------------------------------

type Config struct {
	DLPort      string
	ULPort      string
	UDPPort     string
	ULBufSize   int           // upload read buffer (bytes)
	TCPRecvBuf  int           // SO_RCVBUF for upload socket (bytes)
	IdleTimeout time.Duration // upload idle timeout
}

func loadConfig() Config {
	// Defaults
	c := Config{
		DLPort:      "8080",
		ULPort:      "8081",
		UDPPort:     "7",
		ULBufSize:   1 * 1024 * 1024, // 1 MB
		TCPRecvBuf:  4 * 1024 * 1024, // 4 MB
		IdleTimeout: 10 * time.Second,
	}

	// Load config file from same directory as the executable.
	// The file values are written into the environment so the
	// single env-var block below handles both sources uniformly.
	if exePath, err := os.Executable(); err == nil {
		cfgPath := filepath.Join(filepath.Dir(exePath), "tr143.conf")
		if f, err := os.Open(cfgPath); err == nil {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				if len(parts) != 2 {
					continue
				}
				key := strings.TrimSpace(parts[0])
				val := strings.TrimSpace(parts[1])
				// Strip inline comments
				if idx := strings.IndexByte(val, '#'); idx >= 0 {
					val = strings.TrimSpace(val[:idx])
				}
				// Only set if not already present in the environment
				// (env vars take precedence over the file)
				if os.Getenv(key) == "" {
					os.Setenv(key, val)
				}
			}
		}
	}

	// Apply environment variables
	if v := os.Getenv("DL_PORT"); v != "" {
		c.DLPort = v
	}
	if v := os.Getenv("UL_PORT"); v != "" {
		c.ULPort = v
	}
	if v := os.Getenv("UDP_PORT"); v != "" {
		c.UDPPort = v
	}
	if v := os.Getenv("UL_BUFFER_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.ULBufSize = n
		}
	}
	if v := os.Getenv("TCP_RECV_BUFFER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.TCPRecvBuf = n
		}
	}
	if v := os.Getenv("IDLE_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.IdleTimeout = time.Duration(n) * time.Second
		}
	}

	return c
}

// ---------------------------------------------------------------------
// GLOBALS
// ---------------------------------------------------------------------

var (
	cfg         Config
	downloadBuf []byte // pre-allocated once at startup, shared read-only
)

func init() {
	cfg = loadConfig()

	// Pre-fill download buffer with a simple repeating pattern.
	// Content is irrelevant for a throughput test — only speed matters.
	// Allocated once here instead of per-connection to eliminate heap
	// pressure and avoid crypto/rand overhead on every new client.
	downloadBuf = make([]byte, 1*1024*1024)
	for i := range downloadBuf {
		downloadBuf[i] = byte(i)
	}
}

// ---------------------------------------------------------------------
// 1. RAW TCP UPLOAD SERVER
//    Reads and discards all incoming bytes (blackhole pattern).
//    Sends HTTP 200 OK after the client disconnects.
// ---------------------------------------------------------------------

func startRawUploadServer() {
	listener, err := net.Listen("tcp", ":"+cfg.ULPort)
	if err != nil {
		fmt.Printf("Error starting Upload Server: %v\n", err)
		return
	}
	defer listener.Close()

	fmt.Printf("[*] Raw TCP Upload 'Blackhole'   port=%-6s buf=%dKB  recvbuf=%dMB  idle=%s\n",
		cfg.ULPort,
		cfg.ULBufSize/1024,
		cfg.TCPRecvBuf/1024/1024,
		cfg.IdleTimeout,
	)

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleRawUpload(conn)
	}
}

func handleRawUpload(conn net.Conn) {
	defer conn.Close()

	// Increase the kernel TCP receive buffer for this socket.
	// The default (~128-256 KB on most Linux systems) is too small
	// to sustain 2 Gbps; 4 MB gives comfortable headroom for the
	// bandwidth-delay product at typical RTTs.
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetReadBuffer(cfg.TCPRecvBuf)
	}

	// Set the initial read deadline.
	// We reset it periodically rather than on every Read() call to
	// avoid the per-syscall overhead at high packet rates.
	// resetEvery is 4× the read buffer, so at 2 Gbps with a 1 MB
	// buffer we reset ~60×/sec instead of ~240×/sec.
	resetEvery := int64(cfg.ULBufSize) * 4
	conn.SetReadDeadline(time.Now().Add(cfg.IdleTimeout))

	buf := make([]byte, cfg.ULBufSize)
	var total, lastReset int64

	for {
		n, err := conn.Read(buf)
		if n > 0 {
			total += int64(n)
			if total-lastReset >= resetEvery {
				conn.SetReadDeadline(time.Now().Add(cfg.IdleTimeout))
				lastReset = total
			}
		}
		if err != nil {
			break
		}
	}

	// TR-143 upload test expects an HTTP 200 OK after the data phase.
	conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

// ---------------------------------------------------------------------
// 2. HTTP DOWNLOAD HANDLER
//    Streams the pre-allocated buffer in a tight loop.
//    The fake 1 TB Content-Length keeps the client streaming until
//    it decides to disconnect (controlled by the test duration).
// ---------------------------------------------------------------------

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=test.bin")
	w.Header().Set("Content-Length", "1099511627776") // 1 TB — test runs until client disconnects

	for {
		if _, err := w.Write(downloadBuf); err != nil {
			return // client disconnected
		}
	}
}

// ---------------------------------------------------------------------
// 3. UDP ECHO SERVER
//    Spawns one goroutine per CPU core, all reading from the same
//    socket. This prevents single-packet queuing under burst probes
//    without needing SO_REUSEPORT or multiple sockets.
// ---------------------------------------------------------------------

func startUDPEcho() {
	addr, _ := net.ResolveUDPAddr("udp", ":"+cfg.UDPPort)
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Printf("Error starting UDP Echo: %v\n", err)
		return
	}
	defer conn.Close()

	workers := runtime.NumCPU()
	fmt.Printf("[*] UDP Echo                     port=%-6s workers=%d\n", cfg.UDPPort, workers)

	for i := 0; i < workers; i++ {
		go func() {
			buf := make([]byte, 65535)
			for {
				n, addr, err := conn.ReadFromUDP(buf)
				if err != nil {
					return
				}
				conn.WriteToUDP(buf[:n], addr)
			}
		}()
	}

	select {} // park this goroutine; workers run independently
}

// ---------------------------------------------------------------------
// MAIN
// ---------------------------------------------------------------------

func main() {
	go startUDPEcho()
	go startRawUploadServer()

	mux := http.NewServeMux()
	mux.HandleFunc("/", downloadHandler)

	server := &http.Server{
		Addr:    ":" + cfg.DLPort,
		Handler: mux,
		// Prevent slow-header attacks without killing active download streams.
		// WriteTimeout is intentionally omitted: download tests run for an
		// arbitrary duration determined by the client.
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	fmt.Printf("[*] HTTP Download Server         port=%s\n", cfg.DLPort)

	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("Error starting HTTP server: %v\n", err)
	}
}
