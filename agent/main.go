package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/http2"
)

// Agent holds the configuration for the tunnel client
type Agent struct {
	ID        string
	Subdomain string
	Gateway   string // e.g. "https://tunnel.bar-meet.com"
	LocalHost string // e.g. "localhost:8080"
	Client    *http.Client
}

func main() {
	// Configuration (In "Pro Level" this would be from CLI flags)
	a := &Agent{
		ID:        "agent-unique-id-123",
		Subdomain: "bar-meet-app",
		Gateway:   "http://localhost:9000", // Gateway control port
		LocalHost: "http://localhost:8080", // Local development server
	}

	// Use HTTP/2 transport even for HTTP
	t := &http2.Transport{
		AllowHTTP: true,
		DialTLS: func(network, addr string, cfg *tls.Config) (net.Conn, error) {
			return net.Dial(network, addr)
		},
	}
	a.Client = &http.Client{Transport: t}

	log.Printf("🚀 Starting Bar Meet Tunnel Agent: %s -> %s\n", a.ID, a.Subdomain)

	// Heartbeat / Registration loop
	go a.maintainConnection()

	// Wait for exit
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("🔌 Gracefully shutting down...")
}

func (a *Agent) maintainConnection() {
	for {
		log.Printf("📥 Registering subdomain: %s\n", a.Subdomain)

		req, err := http.NewRequest("POST", a.Gateway+"/register", nil)
		if err != nil {
			log.Fatalf("❌ Failed to create request: %v\n", err)
		}
		req.Header.Set("X-Agent-ID", a.ID)
		req.Header.Set("X-Subdomain", a.Subdomain)

		// Persistent connection loop
		resp, err := a.Client.Do(req)
		if err != nil {
			log.Printf("⚠️ Registration failed: %v. Retrying in 5s...\n", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("✅ Connected to Gateway. Waiting for traffic...\n")

		// If the registration endpoint uses Chunked encoding, we keep reading
		// or wait for context cancellation.
		// For now, if the response ends, we reconnect.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		log.Printf("⚠️ Connection closed by Gateway. Reconnecting in 5s...\n")
		time.Sleep(5 * time.Second)
	}
}

// forwardToLocal function is where we handle incoming proxied requests
// and forward them to localhost.
func (a *Agent) forwardToLocal(w http.ResponseWriter, r *http.Request) {
	// Security: Sanitize path to prevent traversal
	// Use path.Clean to collapse any ".." etc.
	sanitizedPath := path.Clean(r.URL.Path)
	if !strings.HasPrefix(sanitizedPath, "/") {
		sanitizedPath = "/" + sanitizedPath
	}

	u, err := url.Parse(a.LocalHost)
	if err != nil {
		http.Error(w, "Proxy configuration error", 500)
		return
	}
	u.Path = path.Join(u.Path, sanitizedPath)
	u.RawQuery = r.URL.RawQuery
	targetURL := u.String()

	log.Printf("🔄 Forwarding %s -> %s\n", r.URL.Path, targetURL)

	// Proxy the request to localhost
	localReq, err := http.NewRequest(r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "Request construction failed", 500)
		return
	}

	// Copy essential headers only
	for _, h := range []string{"Content-Type", "Accept", "Authorization"} {
		if val := r.Header.Get(h); val != "" {
			localReq.Header.Set(h, val)
		}
	}
	localReq.Header.Set("X-Forwarded-For", r.RemoteAddr)

	localResp, err := http.DefaultClient.Do(localReq)
	if err != nil {
		log.Printf("❌ Failed to reach local service: %v\n", err)
		http.Error(w, "Service Unavailable", 503)
		return
	}
	defer localResp.Body.Close()

	// Stream back to Gateway
	for k, v := range localResp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(localResp.StatusCode)
	io.Copy(w, localResp.Body)
}
