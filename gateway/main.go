package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-redis/redis/v8"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// Gateway holds the state of the tunnel server
type Gateway struct {
	redisClient *redis.Client
	// Map subdomain to the tunnel tunnel
	// key: subdomain, value: agentID
	activeTunnels sync.Map // Map[string]string
	// Map agentID to the connection (stream/transport)
	// For "pro level" we would use a persistent HTTP/2 connection
	agents sync.Map // Map[string]*AgentConn
}

type AgentConn struct {
	ID        string
	ReqChan   chan *ProxyTask
	Transport *http2.Transport
}

type ProxyTask struct {
	Request  *http.Request
	Response chan *http.Response
	Err      chan error
}

func main() {
	g := &Gateway{
		redisClient: redis.NewClient(&redis.Options{
			Addr: "localhost:6379",
		}),
	}

	// 1. Control Server (Agents connect here)
	controlMux := http.NewServeMux()
	controlMux.HandleFunc("/register", g.handleAgentRegistration)

	// 2. Public Proxy Server (Users browse here)
	proxyHandler := http.HandlerFunc(g.handlePublicRequest)

	// Start Control Server (H2C for internal)
	go func() {
		h2s := &http2.Server{}
		server := &http.Server{
			Addr:    ":9000",
			Handler: h2c.NewHandler(controlMux, h2s),
		}
		log.Println("🏗️ Control Server listening on :9000")
		log.Fatal(server.ListenAndServe())
	}()

	// Start Public Server (Handles subdomains)
	h2sProxy := &http2.Server{}
	serverProxy := &http.Server{
		Addr:    ":80",
		Handler: h2c.NewHandler(proxyHandler, h2sProxy),
	}
	log.Println("🌐 Public Proxy Server listening on :80")
	log.Fatal(serverProxy.ListenAndServe())
}

// handleAgentRegistration handles the persistent HTTP/2 connection from the agent
func (g *Gateway) handleAgentRegistration(w http.ResponseWriter, r *http.Request) {
	// Security: Validate subdomain format
	validSubdomain := regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

	agentID := r.Header.Get("X-Agent-ID")
	subdomain := r.Header.Get("X-Subdomain")

	if agentID == "" || subdomain == "" {
		http.Error(w, "Missing credentials", http.StatusUnauthorized)
		return
	}

	if !validSubdomain.MatchString(subdomain) || len(agentID) > 64 {
		http.Error(w, "Invalid input format", http.StatusBadRequest)
		return
	}

	log.Printf("📥 Agent %s registering subdomain: %s\n", agentID, subdomain)

	// Store in Redis (TTL 1 min, will be heartbeat-ed)
	ctx := context.Background()
	err := g.redisClient.Set(ctx, fmt.Sprintf("tunnel:%s", subdomain), agentID, time.Minute).Err()
	if err != nil {
		log.Printf("❌ Redis error: %v\n", err)
		http.Error(w, "Internal error", 500)
		return
	}

	// Maintain persistent connection using Hijacking or Chunked encoding?
	// Actually for HTTP/2 "Pro level", we keep the connection open and use it to PUSH.
    // In this simple version, we'll keep the agent in wait mode.
	w.WriteHeader(http.StatusOK)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Keep connection alive
	<-r.Context().Done()
	log.Printf("🔌 Agent %s disconnected\n", agentID)
	g.redisClient.Del(ctx, fmt.Sprintf("tunnel:%s", subdomain))
}

func (g *Gateway) handlePublicRequest(w http.ResponseWriter, r *http.Request) {
	host := r.Host // e.g. user1.tunnel.com
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		http.Error(w, "Invalid host", 400)
		return
	}
	validSubdomain := regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	subdomain := parts[0]

	if !validSubdomain.MatchString(subdomain) {
		http.Error(w, "Access Denied: Invalid Host", http.StatusForbidden)
		return
	}

	// Lookup agentID in Redis
	ctx := context.Background()
	agentID, err := g.redisClient.Get(ctx, fmt.Sprintf("tunnel:%s", subdomain)).Result()
	if err == redis.Nil {
		http.Error(w, "Tunnel not found or inactive", 404)
		return
	} else if err != nil {
		http.Error(w, "Service Unavailable", 503)
		return
	}

	// We need to proxy this request to the AGENT.
	// This is where "Pro Level" magic happens.
	// For now, I'll outline the proxy logic.
	log.Printf("🚀 Proxying request for %s to Agent %s\n", host, agentID)

	// In a real implementation, we would use an established HTTP/2 transport
	// back to the agent.
	g.proxyToAgent(w, r, agentID)
}

func (g *Gateway) proxyToAgent(w http.ResponseWriter, r *http.Request, agentID string) {
	// Dummy implementation for now - returning placeholder
	// In the real version, we pipe R into the Agent's open connection.
	w.Write([]byte(fmt.Sprintf("Welcome to Bar Meet Tunnel! Mapping to Agent %s works.", agentID)))
}
