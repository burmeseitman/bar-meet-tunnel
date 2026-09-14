package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
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
	Token     string
}

type agentConnection struct {
	agent  *Agent
	conn   *websocket.Conn
	sendMu sync.Mutex
	mu     sync.Mutex
	active map[string]context.CancelFunc
	slots  chan struct{}
}

func main() {
	agent := newAgentFromEnv()
	if len(agent.Token) < 32 {
		log.Fatal("AGENT_TOKEN must contain at least 32 characters")
	}

	if _, err := validateGatewayURL(agent.GatewayWS); err != nil {
		log.Fatal(err)
	}
	log.Printf("starting agent %s for subdomain %s -> %s", agent.ID, agent.Subdomain, agent.LocalHost)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); agent.run(ctx) }()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down agent")
	cancel()
	<-done
}

func newAgentFromEnv() *Agent {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "local"
	}

	timeout := 60 * time.Second
	if value := os.Getenv("REQUEST_TIMEOUT_SECONDS"); value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 1 || seconds > 300 {
			log.Fatal("REQUEST_TIMEOUT_SECONDS must be between 1 and 300")
		}
		timeout = time.Duration(seconds) * time.Second
	}
	return &Agent{
		Token:     os.Getenv("AGENT_TOKEN"),
		ID:        envOrDefault("AGENT_ID", fmt.Sprintf("%s-%d", hostname, os.Getpid())),
		Subdomain: envOrDefault("SUBDOMAIN", defaultSubdomain),
		GatewayWS: envOrDefault("GATEWAY_WS", defaultGatewayWS),
		LocalHost: envOrDefault("LOCAL_HOST", defaultLocalHost),
		Client: &http.Client{
			Timeout:       timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
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

	gatewayURL, err := validateGatewayURL(a.GatewayWS)
	if err != nil {
		return err
	}
	query := gatewayURL.Query()
	query.Set("subdomain", a.Subdomain)
	gatewayURL.RawQuery = query.Encode()
	headers := http.Header{"Authorization": {"Bearer " + a.Token}}
	// Resolve localhost locally without consulting DNS when plaintext is permitted.
	if gatewayURL.Scheme == "ws" && strings.EqualFold(gatewayURL.Hostname(), "localhost") {
		dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		}
	}
	conn, _, err := dialer.DialContext(ctx, gatewayURL.String(), headers)
	if err != nil {
		return fmt.Errorf("dial gateway: %w", err)
	}
	defer conn.Close()

	conn.SetReadLimit(maxBodyBytes*2 + (1 << 20))
	sessionCtx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	session := &agentConnection{agent: a, conn: conn, active: make(map[string]context.CancelFunc), slots: make(chan struct{}, 8)}
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})

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
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"), time.Now().Add(time.Second))
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

		if msg.Type == protocol.MessageTypeCancel {
			session.mu.Lock()
			if cancel := session.active[msg.CancelID]; cancel != nil {
				cancel()
			}
			session.mu.Unlock()
			continue
		}
		if msg.Type != protocol.MessageTypeProxyRequest || msg.Request == nil {
			continue
		}

		select {
		case session.slots <- struct{}{}:
		default:
			if err := session.writeJSON(protocol.Message{Type: protocol.MessageTypeProxyResponse, Response: &protocol.ProxyResponse{ID: msg.Request.ID, StatusCode: http.StatusServiceUnavailable, Body: []byte("agent busy"), CompletedAt: time.Now().UTC()}}); err != nil {
				return err
			}
			continue
		}
		requestCtx, cancel := context.WithCancel(sessionCtx)
		session.mu.Lock()
		if _, exists := session.active[msg.Request.ID]; exists {
			session.mu.Unlock()
			cancel()
			<-session.slots
			return errors.New("duplicate request ID")
		}
		session.active[msg.Request.ID] = cancel
		session.mu.Unlock()
		go func(request *protocol.ProxyRequest) {
			defer func() {
				cancel()
				session.mu.Lock()
				delete(session.active, request.ID)
				session.mu.Unlock()
				<-session.slots
			}()
			session.handleProxyRequest(requestCtx, request)
		}(msg.Request)
	}
}

func (c *agentConnection) handleProxyRequest(parent context.Context, proxyRequest *protocol.ProxyRequest) {
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

	ctx, cancel := context.WithTimeout(parent, c.agent.Client.Timeout)
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

	if base.Scheme != "http" && base.Scheme != "https" || base.Host == "" {
		return "", errors.New("LOCAL_HOST must be an absolute HTTP(S) URL")
	}
	decodedPath, err := url.PathUnescape(requestPath)
	if err != nil {
		return "", fmt.Errorf("invalid escaped path: %w", err)
	}
	target := *base
	target.Path = decodedPath
	target.RawPath = requestPath
	target.RawQuery = rawQuery
	target.Fragment = ""
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
	nominated := make(map[string]bool)
	for _, value := range headers.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			nominated[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}
	for key, values := range headers {
		if nominated[http.CanonicalHeaderKey(key)] {
			continue
		}
		if _, skip := hopByHopHeader[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func applyHeaders(dst http.Header, src map[string][]string) {
	for key, values := range filterHeaders(http.Header(src)) {
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

func validateGatewayURL(raw string) (*url.URL, error) {
	target, err := url.Parse(raw)
	if err != nil || target.Hostname() == "" || target.User != nil || target.Fragment != "" {
		return nil, errors.New("GATEWAY_WS must be an absolute WebSocket URL without credentials or a fragment")
	}
	if target.Scheme == "wss" {
		return target, nil
	}
	ip := net.ParseIP(target.Hostname())
	if target.Scheme == "ws" && (strings.EqualFold(target.Hostname(), "localhost") || ip != nil && ip.IsLoopback()) {
		return target, nil
	}
	return nil, errors.New("GATEWAY_WS requires wss:// for remote connections; ws:// is allowed only on loopback")
}
