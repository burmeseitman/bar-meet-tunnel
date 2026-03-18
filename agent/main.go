package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bar-meet-tunnel/bar-meet-tunnel/internal/protocol"
	"github.com/gorilla/websocket"
)

const (
	defaultGatewayWS = "ws://localhost:9000/agent/connect"
	defaultLocalHost = "http://localhost:8080"
	defaultSubdomain = "bar-meet-app"
	maxBodyBytes     = 10 << 20
)

var hopByHopHeader = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

type Agent struct {
	ID        string
	Subdomain string
	GatewayWS string
	LocalHost string
	Client    *http.Client
}

type agentConnection struct {
	agent  *Agent
	conn   *websocket.Conn
	sendMu sync.Mutex
}

func main() {
	agent := newAgentFromEnv()

	log.Printf("starting agent %s for subdomain %s -> %s", agent.ID, agent.Subdomain, agent.LocalHost)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go agent.run(ctx)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down agent")
	cancel()
	time.Sleep(500 * time.Millisecond)
}

func newAgentFromEnv() *Agent {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}

	return &Agent{
		ID:        envOrDefault("AGENT_ID", fmt.Sprintf("%s-%d", hostname, os.Getpid())),
		Subdomain: envOrDefault("SUBDOMAIN", defaultSubdomain),
		GatewayWS: envOrDefault("GATEWAY_WS", defaultGatewayWS),
		LocalHost: envOrDefault("LOCAL_HOST", defaultLocalHost),
		Client: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

func (a *Agent) run(ctx context.Context) {
	for {
		err := a.connectAndServe(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("agent session ended: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (a *Agent) connectAndServe(ctx context.Context) error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, a.GatewayWS, nil)
	if err != nil {
		return fmt.Errorf("dial gateway: %w", err)
	}
	defer conn.Close()

	conn.SetReadLimit(maxBodyBytes*2 + (1 << 20))
	session := &agentConnection{agent: a, conn: conn}

	if err := session.writeJSON(protocol.Message{
		Type: protocol.MessageTypeHello,
		Hello: &protocol.Hello{
			AgentID:   a.ID,
			Subdomain: a.Subdomain,
			LocalHost: a.LocalHost,
		},
	}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			session.sendMu.Lock()
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"), time.Now().Add(time.Second))
			session.sendMu.Unlock()
			_ = conn.Close()
		case <-done:
		}
	}()
	defer close(done)

	for {
		var msg protocol.Message
		if err := conn.ReadJSON(&msg); err != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return err
		}

		if msg.Type != protocol.MessageTypeProxyRequest || msg.Request == nil {
			continue
		}

		go session.handleProxyRequest(msg.Request)
	}
}

func (c *agentConnection) handleProxyRequest(proxyRequest *protocol.ProxyRequest) {
	startedAt := time.Now().UTC()
	response := &protocol.ProxyResponse{
		ID: proxyRequest.ID,
	}

	targetURL, err := c.agent.buildTargetURL(proxyRequest.Path, proxyRequest.RawQuery)
	if err != nil {
		response.StatusCode = http.StatusBadGateway
		response.Error = err.Error()
		response.CompletedAt = time.Now().UTC()
		response.DurationMS = time.Since(startedAt).Milliseconds()
		_ = c.writeJSON(protocol.Message{Type: protocol.MessageTypeProxyResponse, Response: response})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	localReq, err := http.NewRequestWithContext(ctx, proxyRequest.Method, targetURL, bytes.NewReader(proxyRequest.Body))
	if err != nil {
		response.StatusCode = http.StatusBadGateway
		response.Error = fmt.Sprintf("build local request: %v", err)
		response.CompletedAt = time.Now().UTC()
		response.DurationMS = time.Since(startedAt).Milliseconds()
		_ = c.writeJSON(protocol.Message{Type: protocol.MessageTypeProxyResponse, Response: response})
		return
	}

	applyHeaders(localReq.Header, proxyRequest.Headers)
	localReq.Host = localReq.URL.Host
	localReq.Header.Set("X-Forwarded-For", proxyRequest.RemoteAddr)
	localReq.Header.Set("X-Forwarded-Host", proxyRequest.Host)
	localReq.Header.Set("X-Tunnel-Subdomain", c.agent.Subdomain)
	if proxyRequest.ReplayOf != "" {
		localReq.Header.Set("X-Bar-Meet-Replay-Of", proxyRequest.ReplayOf)
	}

	localResp, err := c.agent.Client.Do(localReq)
	if err != nil {
		response.StatusCode = http.StatusBadGateway
		response.Error = fmt.Sprintf("reach local service: %v", err)
		response.CompletedAt = time.Now().UTC()
		response.DurationMS = time.Since(startedAt).Milliseconds()
		_ = c.writeJSON(protocol.Message{Type: protocol.MessageTypeProxyResponse, Response: response})
		return
	}
	defer localResp.Body.Close()

	body, err := readBody(localResp.Body, maxBodyBytes)
	if err != nil {
		response.StatusCode = http.StatusBadGateway
		response.Error = err.Error()
		response.CompletedAt = time.Now().UTC()
		response.DurationMS = time.Since(startedAt).Milliseconds()
		_ = c.writeJSON(protocol.Message{Type: protocol.MessageTypeProxyResponse, Response: response})
		return
	}

	response.StatusCode = localResp.StatusCode
	response.Headers = filterHeaders(localResp.Header)
	response.Body = body
	response.CompletedAt = time.Now().UTC()
	response.DurationMS = time.Since(startedAt).Milliseconds()

	if err := c.writeJSON(protocol.Message{
		Type:     protocol.MessageTypeProxyResponse,
		Response: response,
	}); err != nil {
		log.Printf("send proxy response failed: %v", err)
	}
}

func (a *Agent) buildTargetURL(requestPath, rawQuery string) (string, error) {
	base, err := url.Parse(a.LocalHost)
	if err != nil {
		return "", fmt.Errorf("parse local host: %w", err)
	}

	if requestPath == "" {
		requestPath = "/"
	}

	target := base.ResolveReference(&url.URL{
		Path:     requestPath,
		RawQuery: rawQuery,
	})
	return target.String(), nil
}

func (c *agentConnection) writeJSON(msg protocol.Message) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(msg)
}

func readBody(body io.ReadCloser, limit int64) ([]byte, error) {
	reader := io.LimitReader(body, limit+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("body exceeds %d bytes", limit)
	}
	return data, nil
}

func filterHeaders(headers http.Header) map[string][]string {
	cloned := make(map[string][]string, len(headers))
	for key, values := range headers {
		if _, skip := hopByHopHeader[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func applyHeaders(dst http.Header, src map[string][]string) {
	for key, values := range src {
		if _, skip := hopByHopHeader[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
