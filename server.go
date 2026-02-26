package main

import (
        "crypto/rand"
        "fmt"
        "net"
        "net/http"
        "time"
)

// CONFIGURATION
const (
        DL_PORT     = "8080" // HTTP Download
        UL_PORT     = "8081" // Raw TCP Upload (The "Blackhole")
        UDP_PORT    = "7"    // UDP Echo
)

// ---------------------------------------------------------------------
// 1. RAW TCP UPLOAD SERVER (Port 8081)
//    Mimics the Python script: Bypasses HTTP parsing overhead.
// ---------------------------------------------------------------------
func startRawUploadServer() {
        listener, err := net.Listen("tcp", ":"+UL_PORT)
        if err != nil {
                fmt.Printf("Error starting Upload Server: %v\n", err)
                return
        }
        defer listener.Close()
        fmt.Printf("[*] Raw TCP Upload 'Blackhole' listening on port %s\n", UL_PORT)

        for {
                conn, err := listener.Accept()
                if err != nil {
                        continue
                }
                // Handle each connection in a lightweight goroutine
                go handleRawUpload(conn)
        }
}

func handleRawUpload(conn net.Conn) {
        defer conn.Close()

        // Safety timeout: Close connection if no data for 10 seconds
        conn.SetDeadline(time.Now().Add(10 * time.Second))

        // 1. Read/Discard loop
        // We use a 32KB buffer, same as the Python script
        buf := make([]byte, 32*1024)
        for {
                // Read data from the socket
                _, err := conn.Read(buf)
                if err != nil {
                        // EOF (Router finished) or Timeout
                        break
                }
                // We successfully read bytes. Reset the timeout timer
                // so we don't kill a long-running active test.
                conn.SetDeadline(time.Now().Add(10 * time.Second))
        }

        // 2. Send "200 OK" manually
        // Since we bypassed the HTTP server, we must type out the response.
        response := "HTTP/1.1 200 OK\r\n" +
                "Content-Type: text/plain\r\n" +
                "Content-Length: 0\r\n" +
                "Connection: close\r\n" +
                "\r\n"

        conn.Write([]byte(response))
}

// ---------------------------------------------------------------------
// 2. HTTP DOWNLOAD HANDLER (Port 8080)
// ---------------------------------------------------------------------
func downloadHandler(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/octet-stream")
        w.Header().Set("Content-Disposition", "attachment; filename=test.bin")
        // Fake 1 TB size so it never finishes
        w.Header().Set("Content-Length", "1099511627776")

        // 1MB buffer of random data
        buffer := make([]byte, 1024*1024)
        rand.Read(buffer)

        for {
                _, err := w.Write(buffer)
                if err != nil {
                        return // Client disconnected
                }
        }
}

// ---------------------------------------------------------------------
// 3. UDP ECHO SERVER (Port 7)
// ---------------------------------------------------------------------
func startUDPEcho() {
        addr, _ := net.ResolveUDPAddr("udp", ":"+UDP_PORT)
        conn, err := net.ListenUDP("udp", addr)
        if err != nil {
                fmt.Printf("Error starting UDP Echo: %v\n", err)
                return
        }
        defer conn.Close()

        fmt.Printf("[*] UDP Echo listening on port %s\n", UDP_PORT)

        buf := make([]byte, 65535)
        for {
                n, clientAddr, err := conn.ReadFromUDP(buf)
                if err != nil {
                        continue
                }
                conn.WriteToUDP(buf[:n], clientAddr)
        }
}

// ---------------------------------------------------------------------
// MAIN
// ---------------------------------------------------------------------
func main() {
        // Start UDP Echo and Raw Upload in separate goroutines
        go startUDPEcho()
        go startRawUploadServer()

        // Start HTTP Download Server (Blocking)
        http.HandleFunc("/", downloadHandler)

        fmt.Printf("[*] HTTP Download Server listening on port %s\n", DL_PORT)

        if err := http.ListenAndServe(":"+DL_PORT, nil); err != nil {
                fmt.Printf("Error starting HTTP server: %v\n", err)
        }
}
