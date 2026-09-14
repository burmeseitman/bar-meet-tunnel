package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bar-meet-tunnel/bar-meet-tunnel/internal/protocol"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
)

const (
	defaultPublicAddr     = ":80"
	defaultControlAddr    = "127.0.0.1:9000"
	defaultRequestTimeout = 60 * time.Second
	maxHistoryBytes       = 64 << 20
	maxConcurrentRequests = 32
	maxBodyBytes          = 10 << 20
	maxTrafficRecords     = 250
	redisTunnelTTL        = 90 * time.Second
)

var (
	errTunnelBusy  = errors.New("tunnel busy")
	validSubdomain = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	hopByHopHeader = map[string]struct{}{
		"Connection":          {},
		"Keep-Alive":          {},
		"Proxy-Authenticate":  {},
		"Proxy-Authorization": {},
		"Te":                  {},
		"Trailer":             {},
		"Transfer-Encoding":   {},
		"Upgrade":             {},
	}
	websocketUpgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
	}
)

//go:embed ui/index.html
var uiFS embed.FS

type Gateway struct {
	redisClient    *redis.Client
	controlToken   string
	agentTokens    map[string]string
	trustedProxies []*net.IPNet
	timeout        time.Duration
	slots          chan struct{}
	historyBytes   int64
	captureMode    string
	captureTTL     time.Duration
	redactExtra    []string
	clients        clientLimiter

	mu                  sync.RWMutex
	sessionsBySubdomain map[string]*AgentSession
	sessionsByAgentID   map[string]*AgentSession
	trafficByID         map[string]*TrafficRecord
	trafficOrder        []string

	sequence atomic.Uint64
}

type AgentSession struct {
	gateway *Gateway
	conn    *websocket.Conn

	agentID   string
	subdomain string
	localHost string
	slots     chan struct{}

	connectedAt time.Time
	lastSeenNS  atomic.Int64
	requests    atomic.Uint64
	bytesIn     atomic.Uint64
	bytesOut    atomic.Uint64

	sendMu    sync.Mutex
	pendingMu sync.Mutex
	pending   map[string]chan *protocol.ProxyResponse

	closed    chan struct{}
	closeOnce sync.Once
}

type RequestSnapshot struct {
	Method     string
	Path       string
	RawQuery   string
	Host       string
	RemoteAddr string
	Headers    map[string][]string
	Body       []byte
	ReplayOf   string
}

type TrafficRecord struct {
	Replayable      bool
	ExpiresAt       time.Time
	ID              string
	Tunnel          string
	AgentID         string
	Method          string
	Path            string
	RawQuery        string
	Host            string
	RemoteAddr      string
	StatusCode      int
	Error           string
	DurationMS      int64
	StartedAt       time.Time
	CompletedAt     time.Time
	RequestHeaders  map[string][]string
	ResponseHeaders map[string][]string
	RequestBody     []byte
	ResponseBody    []byte
	ReplayOf        string
}

type BodyView struct {
	Content   string `json:"content"`
	Encoding  string `json:"encoding"`
	Size      int    `json:"size"`
	Truncated bool   `json:"truncated"`
}

type TrafficRecordView struct {
	Replayable      bool                `json:"replayable"`
	ID              string              `json:"id"`
	Tunnel          string              `json:"tunnel"`
	AgentID         string              `json:"agent_id"`
	Method          string              `json:"method"`
	Path            string              `json:"path"`
	RawQuery        string              `json:"raw_query,omitempty"`
	Host            string              `json:"host"`
	RemoteAddr      string              `json:"remote_addr,omitempty"`
	StatusCode      int                 `json:"status_code"`
	Error           string              `json:"error,omitempty"`
	DurationMS      int64               `json:"duration_ms"`
	StartedAt       time.Time           `json:"started_at"`
	CompletedAt     time.Time           `json:"completed_at"`
	RequestHeaders  map[string][]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
	RequestBody     BodyView            `json:"request_body"`
	ResponseBody    BodyView            `json:"response_body"`
	ReplayOf        string              `json:"replay_of,omitempty"`
}

type TunnelView struct {
	Subdomain    string    `json:"subdomain"`
	AgentID      string    `json:"agent_id"`
	LocalHost    string    `json:"local_host"`
	ConnectedAt  time.Time `json:"connected_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	RequestCount uint64    `json:"request_count"`
	BytesIn      uint64    `json:"bytes_in"`
	BytesOut     uint64    `json:"bytes_out"`
}

func main() {
	gateway := newGateway()
	gateway.loadConfig()

	controlMux := http.NewServeMux()
	controlMux.HandleFunc("/agent/connect", gateway.handleAgentConnect)
	controlMux.HandleFunc("/api/tunnels", gateway.handleAPITunnels)
	controlMux.HandleFunc("/api/requests", gateway.handleAPIRequests)
	controlMux.HandleFunc("/api/requests/", gateway.handleAPIRequestByID)
	controlMux.HandleFunc("/healthz", gateway.handleHealth)
	controlMux.HandleFunc("/", gateway.handleUI)

	publicMux := http.NewServeMux()
	publicMux.HandleFunc("/", gateway.handlePublicRequest)

	control := &http.Server{Addr: envOrDefault("CONTROL_ADDR", defaultControlAddr), Handler: gateway.controlHandler(controlMux), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: gateway.timeout, WriteTimeout: 2*gateway.timeout + 15*time.Second, IdleTimeout: 60 * time.Second}
	public := &http.Server{Addr: envOrDefault("PUBLIC_ADDR", defaultPublicAddr), Handler: publicMux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: gateway.timeout, WriteTimeout: 2*gateway.timeout + 15*time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go gateway.expireTrafficLoop(ctx)
	failures := make(chan error, 2)
	for _, server := range []*http.Server{control, public} {
		go func() { log.Printf("listening on %s", server.Addr); failures <- server.ListenAndServe() }()
	}
	select {
	case <-ctx.Done():
	case err := <-failures:
		log.Printf("server stopped: %v", err)
	}
	gateway.mu.RLock()
	sessions := make([]*AgentSession, 0, len(gateway.sessionsBySubdomain))
	for _, session := range gateway.sessionsBySubdomain {
		sessions = append(sessions, session)
	}
	gateway.mu.RUnlock()
	for _, session := range sessions {
		session.close(websocket.CloseGoingAway, "gateway shutdown")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, server := range []*http.Server{control, public} {
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
		}
	}
	if gateway.redisClient != nil {
		_ = gateway.redisClient.Close()
	}

}

func newGateway() *Gateway {
	gateway := &Gateway{
		sessionsBySubdomain: make(map[string]*AgentSession),
		sessionsByAgentID:   make(map[string]*AgentSession),
		trafficByID:         make(map[string]*TrafficRecord),
		timeout:             defaultRequestTimeout,
		captureMode:         "metadata",
		captureTTL:          15 * time.Minute,
		slots:               make(chan struct{}, maxConcurrentRequests),
	}

	redisAddr := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	client := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		log.Printf("redis unavailable at %s: %v; continuing with in-memory registry", redisAddr, err)
		return gateway
	}

	gateway.redisClient = client
	log.Printf("connected to redis at %s", redisAddr)
	return gateway
}

func (g *Gateway) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	subdomain := r.URL.Query().Get("subdomain")
	if !validSubdomain.MatchString(subdomain) || !tokenEqual(bearerToken(r), g.agentTokens[subdomain]) {
		http.Error(w, "unauthorized agent", http.StatusUnauthorized)
		return
	}
	conn, err := websocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("agent websocket upgrade failed: %v", err)
		return
	}

	conn.SetReadLimit(4096)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

	var hello protocol.Message
	if err := conn.ReadJSON(&hello); err != nil {
		log.Printf("agent hello failed: %v", err)
		_ = conn.Close()
		return
	}

	if hello.Type != protocol.MessageTypeHello || hello.Hello == nil {
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "hello message required"),
			time.Now().Add(time.Second),
		)
		_ = conn.Close()
		return
	}

	if !validSubdomain.MatchString(hello.Hello.Subdomain) || hello.Hello.AgentID == "" || hello.Hello.Subdomain != subdomain {
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "invalid agent metadata"),
			time.Now().Add(time.Second),
		)
		_ = conn.Close()
		return
	}

	conn.SetReadLimit(maxBodyBytes*2 + (1 << 20))
	session := &AgentSession{
		gateway:     g,
		conn:        conn,
		agentID:     hello.Hello.AgentID,
		subdomain:   hello.Hello.Subdomain,
		localHost:   hello.Hello.LocalHost,
		connectedAt: time.Now().UTC(),
		pending:     make(map[string]chan *protocol.ProxyResponse),
		closed:      make(chan struct{}),
		slots:       make(chan struct{}, 8),
	}
	session.touch()

	conn.SetPongHandler(func(string) error {
		session.touch()
		return conn.SetReadDeadline(time.Now().Add(90 * time.Second))
	})
	_ = conn.SetReadDeadline(time.Now().Add(90 * time.Second))

	if !g.registerSession(session) {
		session.close(websocket.ClosePolicyViolation, "subdomain or agent ID already connected")
		return
	}
	defer g.unregisterSession(session)

	log.Printf("agent connected: %s (%s -> %s)", session.agentID, session.subdomain, session.localHost)

	go session.keepAliveLoop()
	go session.pingLoop()

	session.readLoop()
}

func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	g.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (g *Gateway) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.Redirect(w, r, "/ui", http.StatusTemporaryRedirect)
		return
	}
	if r.URL.Path != "/ui" {
		http.NotFound(w, r)
		return
	}

	content, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, "ui unavailable", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(content)
}

func (g *Gateway) handleAPITunnels(w http.ResponseWriter, _ *http.Request) {
	g.mu.RLock()
	tunnels := make([]TunnelView, 0, len(g.sessionsBySubdomain))
	for _, session := range g.sessionsBySubdomain {
		tunnels = append(tunnels, TunnelView{
			Subdomain:    session.subdomain,
			AgentID:      session.agentID,
			LocalHost:    session.localHost,
			ConnectedAt:  session.connectedAt,
			LastSeenAt:   session.lastSeenAt(),
			RequestCount: session.requests.Load(),
			BytesIn:      session.bytesIn.Load(),
			BytesOut:     session.bytesOut.Load(),
		})
	}
	g.mu.RUnlock()

	sort.Slice(tunnels, func(i, j int) bool {
		return tunnels[i].Subdomain < tunnels[j].Subdomain
	})

	g.writeJSON(w, http.StatusOK, tunnels)
}

func (g *Gateway) handleAPIRequests(w http.ResponseWriter, r *http.Request) {
	g.expireTraffic(time.Now())
	filterTunnel := strings.TrimSpace(r.URL.Query().Get("tunnel"))

	g.mu.RLock()
	records := make([]TrafficRecordView, 0, len(g.trafficOrder))
	for i := len(g.trafficOrder) - 1; i >= 0; i-- {
		record := g.trafficByID[g.trafficOrder[i]]
		if record == nil {
			continue
		}
		if filterTunnel != "" && record.Tunnel != filterTunnel {
			continue
		}
		records = append(records, g.trafficView(record))
	}
	g.mu.RUnlock()

	g.writeJSON(w, http.StatusOK, records)
}

func (g *Gateway) handleAPIRequestByID(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/requests/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		http.NotFound(w, r)
		return
	}

	recordID := parts[0]

	if len(parts) == 1 && r.Method == http.MethodGet {
		record, ok := g.getTraffic(recordID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		g.writeJSON(w, http.StatusOK, g.trafficView(record))
		return
	}

	if len(parts) == 2 && parts[1] == "replay" && r.Method == http.MethodPost {
		record, ok := g.getTraffic(recordID)
		if !ok {
			http.NotFound(w, r)
			return
		}

		if !record.Replayable {
			http.Error(w, "replay requires full traffic capture", http.StatusConflict)
			return
		}
		session, ok := g.lookupSession(record.Tunnel)
		if !ok {
			http.Error(w, "tunnel is not active", http.StatusConflict)
			return
		}

		release, err := g.admit(session)
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer release()
		replayed, _, err := g.dispatchAdmitted(r.Context(), session, RequestSnapshot{
			Method:     record.Method,
			Path:       record.Path,
			RawQuery:   record.RawQuery,
			Host:       record.Host,
			RemoteAddr: record.RemoteAddr,
			Headers:    cloneHeaderMap(record.RequestHeaders),
			Body:       append([]byte(nil), record.RequestBody...),
			ReplayOf:   record.ID,
		})
		if err != nil {
			http.Error(w, err.Error(), proxyErrorStatus(err))
			return
		}

		g.writeJSON(w, http.StatusOK, g.trafficView(replayed))
		return
	}

	http.NotFound(w, r)
}

func (g *Gateway) handlePublicRequest(w http.ResponseWriter, r *http.Request) {
	releaseIP, ok := g.clients.admit("public:"+g.clientAddress(r), time.Now(), true)
	if !ok {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "client rate or concurrency limit exceeded", http.StatusTooManyRequests)
		return
	}
	defer releaseIP()
	subdomain, ok := extractSubdomain(r.Host)
	if !ok {
		http.Error(w, "invalid host", http.StatusBadRequest)
		return
	}

	session, ok := g.lookupSession(subdomain)
	if !ok {
		http.Error(w, "tunnel not found or inactive", http.StatusNotFound)
		return
	}

	release, err := g.admit(session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer release()
	body, err := readBody(r.Body, maxBodyBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	record, response, err := g.dispatchAdmitted(r.Context(), session, RequestSnapshot{
		Method:     r.Method,
		Path:       requestPath(r),
		RawQuery:   r.URL.RawQuery,
		Host:       r.Host,
		RemoteAddr: g.clientAddress(r),
		Headers:    filterHeaders(r.Header),
		Body:       body,
	})
	if err != nil {
		http.Error(w, err.Error(), proxyErrorStatus(err))
		return
	}

	if response.Error != "" {
		http.Error(w, response.Error, http.StatusBadGateway)
		return
	}

	applyHeaders(w.Header(), response.Headers)
	w.WriteHeader(response.StatusCode)
	if _, err := w.Write(response.Body); err != nil {
		log.Printf("write public response failed for %s: %v", record.ID, err)
	}
}

func (g *Gateway) dispatchRequest(ctx context.Context, session *AgentSession, snapshot RequestSnapshot) (*TrafficRecord, *protocol.ProxyResponse, error) {
	release, err := g.admit(session)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	return g.dispatchAdmitted(ctx, session, snapshot)
}
func (g *Gateway) dispatchAdmitted(ctx context.Context, session *AgentSession, snapshot RequestSnapshot) (*TrafficRecord, *protocol.ProxyResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	requestID := g.nextID("req")
	startedAt := time.Now().UTC()
	msg := &protocol.ProxyRequest{
		ID:         requestID,
		Method:     snapshot.Method,
		Path:       snapshot.Path,
		RawQuery:   snapshot.RawQuery,
		Host:       snapshot.Host,
		RemoteAddr: snapshot.RemoteAddr,
		Headers:    cloneHeaderMap(snapshot.Headers),
		Body:       append([]byte(nil), snapshot.Body...),
		ReplayOf:   snapshot.ReplayOf,
		StartedAt:  startedAt,
	}

	waitCh := session.registerPending(requestID)
	if err := session.sendRequest(msg); err != nil {
		session.takePending(requestID)
		record := g.recordTraffic(session, snapshot, &protocol.ProxyResponse{
			ID:          requestID,
			StatusCode:  http.StatusBadGateway,
			Error:       fmt.Sprintf("send request to agent: %v", err),
			CompletedAt: time.Now().UTC(),
		}, startedAt)
		return record, nil, errors.New(record.Error)
	}

	var response *protocol.ProxyResponse
	select {
	case response = <-waitCh:
	case <-session.closed:
		session.takePending(requestID)
		record := g.recordTraffic(session, snapshot, &protocol.ProxyResponse{
			ID:          requestID,
			StatusCode:  http.StatusBadGateway,
			Error:       "agent disconnected",
			CompletedAt: time.Now().UTC(),
		}, startedAt)
		return record, nil, errors.New(record.Error)
	case <-ctx.Done():
		_ = session.sendMessage(protocol.Message{Type: protocol.MessageTypeCancel, CancelID: requestID})
		session.takePending(requestID)
		record := g.recordTraffic(session, snapshot, &protocol.ProxyResponse{
			ID:          requestID,
			StatusCode:  http.StatusGatewayTimeout,
			Error:       ctx.Err().Error(),
			CompletedAt: time.Now().UTC(),
		}, startedAt)
		return record, nil, ctx.Err()
	}

	if response == nil {
		record := g.recordTraffic(session, snapshot, &protocol.ProxyResponse{
			ID:          requestID,
			StatusCode:  http.StatusBadGateway,
			Error:       "empty response from agent",
			CompletedAt: time.Now().UTC(),
		}, startedAt)
		return record, nil, errors.New(record.Error)
	}

	if response.StatusCode < 200 || response.StatusCode > 599 || len(response.Body) > maxBodyBytes {
		response = &protocol.ProxyResponse{ID: requestID, StatusCode: http.StatusBadGateway, Error: "invalid agent response"}
	}
	record := g.recordTraffic(session, snapshot, response, startedAt)
	session.requests.Add(1)
	session.bytesIn.Add(uint64(len(snapshot.Body)))
	session.bytesOut.Add(uint64(len(response.Body)))
	return record, response, nil
}

func (g *Gateway) recordTraffic(session *AgentSession, snapshot RequestSnapshot, response *protocol.ProxyResponse, startedAt time.Time) *TrafficRecord {
	completedAt := response.CompletedAt
	if completedAt.IsZero() {
		completedAt = time.Now().UTC()
	}

	record := &TrafficRecord{
		ID:          response.ID,
		Tunnel:      session.subdomain,
		AgentID:     session.agentID,
		Method:      snapshot.Method,
		Path:        snapshot.Path,
		RawQuery:    snapshot.RawQuery,
		Host:        snapshot.Host,
		RemoteAddr:  snapshot.RemoteAddr,
		StatusCode:  response.StatusCode,
		Error:       response.Error,
		DurationMS:  durationMillis(startedAt, completedAt, response.DurationMS),
		StartedAt:   startedAt,
		CompletedAt: completedAt,

		ReplayOf: snapshot.ReplayOf,
	}

	if g.captureMode == "off" {
		return record
	}
	record.ExpiresAt = time.Now().Add(g.captureTTL)
	record.RequestHeaders = g.redactHeaders(snapshot.Headers)
	record.ResponseHeaders = g.redactHeaders(response.Headers)
	record.RawQuery = ""
	if g.captureMode == "full" {
		record.Replayable = true
		record.RawQuery = snapshot.RawQuery
		record.RequestHeaders = cloneHeaderMap(snapshot.Headers)
		record.ResponseHeaders = cloneHeaderMap(response.Headers)
		record.RequestBody = append([]byte(nil), snapshot.Body...)
		record.ResponseBody = append([]byte(nil), response.Body...)
	}
	g.mu.Lock()
	g.trafficByID[record.ID] = record
	g.trafficOrder = append(g.trafficOrder, record.ID)
	g.historyBytes += trafficSize(record)
	for len(g.trafficOrder) > maxTrafficRecords || g.historyBytes > maxHistoryBytes {
		oldestID := g.trafficOrder[0]
		g.trafficOrder = g.trafficOrder[1:]
		g.historyBytes -= trafficSize(g.trafficByID[oldestID])
		delete(g.trafficByID, oldestID)
	}
	g.mu.Unlock()

	return record
}

func (g *Gateway) registerSession(session *AgentSession) bool {
	g.mu.Lock()
	if g.sessionsBySubdomain[session.subdomain] != nil || g.sessionsByAgentID[session.agentID] != nil {
		g.mu.Unlock()
		return false
	}
	g.sessionsBySubdomain[session.subdomain] = session
	g.sessionsByAgentID[session.agentID] = session
	g.mu.Unlock()
	g.refreshRedis(session)
	return true
}

func (g *Gateway) unregisterSession(session *AgentSession) {
	session.close(websocket.CloseNormalClosure, "session closed")

	removedSubdomain := false

	g.mu.Lock()
	if current := g.sessionsBySubdomain[session.subdomain]; current == session {
		delete(g.sessionsBySubdomain, session.subdomain)
		removedSubdomain = true
	}
	if current := g.sessionsByAgentID[session.agentID]; current == session {
		delete(g.sessionsByAgentID, session.agentID)
	}
	g.mu.Unlock()

	if removedSubdomain {
		g.removeRedis(session)
	}
	log.Printf("agent disconnected: %s (%s)", session.agentID, session.subdomain)
}

func (g *Gateway) lookupSession(subdomain string) (*AgentSession, bool) {
	g.mu.RLock()
	session, ok := g.sessionsBySubdomain[subdomain]
	g.mu.RUnlock()
	return session, ok
}

func (g *Gateway) getTraffic(id string) (*TrafficRecord, bool) {
	g.expireTraffic(time.Now())
	g.mu.RLock()
	record, ok := g.trafficByID[id]
	g.mu.RUnlock()
	return record, ok
}

func (g *Gateway) refreshRedis(session *AgentSession) {
	if g.redisClient == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := g.redisClient.Set(ctx, redisKey(session.subdomain), session.redisLease(), redisTunnelTTL).Err(); err != nil {
		log.Printf("redis set failed for %s: %v", session.subdomain, err)
	}
}

func (g *Gateway) removeRedis(session *AgentSession) {
	if g.redisClient == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Compare and delete atomically so an old disconnect cannot erase a new lease.
	script := redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`)
	if err := script.Run(ctx, g.redisClient, []string{redisKey(session.subdomain)}, session.redisLease()).Err(); err != nil {
		log.Printf("redis delete failed for %s: %v", session.subdomain, err)
	}

}

func (g *Gateway) nextID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, g.sequence.Add(1))
}

func (g *Gateway) writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("write json failed: %v", err)
	}
}

func (s *AgentSession) keepAliveLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.gateway.refreshRedis(s)
		case <-s.closed:
			return
		}
	}
}

func (s *AgentSession) pingLoop() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second)); err != nil {
				s.close(websocket.CloseGoingAway, "ping failed")
				return
			}
		case <-s.closed:
			return
		}
	}
}

func (s *AgentSession) readLoop() {
	defer s.close(websocket.CloseNormalClosure, "read loop ended")

	for {
		var msg protocol.Message
		if err := s.conn.ReadJSON(&msg); err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("agent read failed for %s: %v", s.agentID, err)
			}
			return
		}

		s.touch()

		if msg.Type != protocol.MessageTypeProxyResponse || msg.Response == nil {
			continue
		}

		if ch := s.takePending(msg.Response.ID); ch != nil {
			ch <- msg.Response
		}
	}
}

func (s *AgentSession) sendRequest(request *protocol.ProxyRequest) error {
	return s.sendMessage(protocol.Message{Type: protocol.MessageTypeProxyRequest, Request: request})
}

func (s *AgentSession) sendMessage(msg protocol.Message) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()

	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return s.conn.WriteJSON(msg)
}

func (s *AgentSession) registerPending(id string) chan *protocol.ProxyResponse {
	ch := make(chan *protocol.ProxyResponse, 1)
	s.pendingMu.Lock()
	s.pending[id] = ch
	s.pendingMu.Unlock()
	return ch
}

func (s *AgentSession) takePending(id string) chan *protocol.ProxyResponse {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	ch := s.pending[id]
	delete(s.pending, id)
	return ch
}

func (s *AgentSession) touch() {
	s.lastSeenNS.Store(time.Now().UTC().UnixNano())
}

func (s *AgentSession) lastSeenAt() time.Time {
	ns := s.lastSeenNS.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

func (s *AgentSession) close(code int, text string) {
	s.closeOnce.Do(func() {
		close(s.closed)
		s.failPending(errors.New("agent disconnected"))

		_ = s.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(time.Second))
		_ = s.conn.Close()
	})
}

func (s *AgentSession) failPending(err error) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()

	for id, ch := range s.pending {
		ch <- &protocol.ProxyResponse{
			ID:          id,
			StatusCode:  http.StatusBadGateway,
			Error:       err.Error(),
			CompletedAt: time.Now().UTC(),
		}
		delete(s.pending, id)
	}
}

func (r *TrafficRecord) view() TrafficRecordView {
	return TrafficRecordView{
		Replayable:      r.Replayable,
		ID:              r.ID,
		Tunnel:          r.Tunnel,
		AgentID:         r.AgentID,
		Method:          r.Method,
		Path:            r.Path,
		RawQuery:        r.RawQuery,
		Host:            r.Host,
		RemoteAddr:      r.RemoteAddr,
		StatusCode:      r.StatusCode,
		Error:           r.Error,
		DurationMS:      r.DurationMS,
		StartedAt:       r.StartedAt,
		CompletedAt:     r.CompletedAt,
		RequestHeaders:  redactHeaders(r.RequestHeaders),
		ResponseHeaders: redactHeaders(r.ResponseHeaders),
		RequestBody:     previewBody(r.RequestBody),
		ResponseBody:    previewBody(r.ResponseBody),
		ReplayOf:        r.ReplayOf,
	}
}

func previewBody(body []byte) BodyView {
	const previewLimit = 8192

	if len(body) == 0 {
		return BodyView{Encoding: "text", Size: 0}
	}

	truncated := len(body) > previewLimit
	sample := body
	if truncated {
		sample = body[:previewLimit]
	}

	if utf8.Valid(sample) {
		return BodyView{
			Content:   string(sample),
			Encoding:  "text",
			Size:      len(body),
			Truncated: truncated,
		}
	}

	return BodyView{
		Content:   base64.StdEncoding.EncodeToString(sample),
		Encoding:  "base64",
		Size:      len(body),
		Truncated: truncated,
	}
}

func readBody(body io.ReadCloser, limit int64) ([]byte, error) {
	defer body.Close()

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

func extractSubdomain(host string) (string, bool) {
	host = stripPort(host)
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return "", false
	}

	subdomain := parts[0]
	if !validSubdomain.MatchString(subdomain) {
		return "", false
	}
	return subdomain, true
}

func stripPort(host string) string {
	if strings.Count(host, ":") == 0 {
		return host
	}
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		return parsedHost
	}
	return strings.Split(host, ":")[0]
}

func requestPath(r *http.Request) string {
	if r.URL.Path == "" {
		return "/"
	}
	return r.URL.EscapedPath()
}

func (g *Gateway) clientAddress(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	trusted := func(value string) bool {
		ip := net.ParseIP(value)
		for _, network := range g.trustedProxies {
			if network.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return peer
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(chain) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(chain[i])
		if net.ParseIP(candidate) == nil {
			return peer
		}
		peer = candidate
		if !trusted(peer) {
			break
		}
	}
	return peer
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

func cloneHeaderMap(headers map[string][]string) map[string][]string {
	if len(headers) == 0 {
		return nil
	}
	cloned := make(map[string][]string, len(headers))
	for key, values := range headers {
		cloned[key] = append([]string(nil), values...)
	}
	return cloned
}

func redisKey(subdomain string) string {
	return "tunnel:" + subdomain
}

func durationMillis(startedAt, completedAt time.Time, fallback int64) int64 {
	if !startedAt.IsZero() && !completedAt.IsZero() {
		return completedAt.Sub(startedAt).Milliseconds()
	}
	return fallback
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func tokenEqual(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
func bearerToken(r *http.Request) string {
	value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return value
}
func (g *Gateway) loadConfig() {
	g.captureMode = envOrDefault("CAPTURE_MODE", "metadata")
	if g.captureMode != "off" && g.captureMode != "metadata" && g.captureMode != "full" {
		log.Fatal("CAPTURE_MODE must be off, metadata, or full")
	}
	ttl, err := time.ParseDuration(envOrDefault("CAPTURE_TTL", "15m"))
	if err != nil || ttl < time.Second || ttl > 24*time.Hour {
		log.Fatal("CAPTURE_TTL must be between 1s and 24h")
	}
	g.captureTTL = ttl
	for _, name := range strings.Split(os.Getenv("REDACT_HEADERS"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			g.redactExtra = append(g.redactExtra, name)
		}
	}
	g.controlToken = os.Getenv("CONTROL_TOKEN")
	if len(g.controlToken) < 32 {
		log.Fatal("CONTROL_TOKEN must contain at least 32 characters")
	}
	if err := json.Unmarshal([]byte(os.Getenv("AGENT_TOKENS")), &g.agentTokens); err != nil || len(g.agentTokens) == 0 {
		log.Fatal("AGENT_TOKENS must be a JSON object mapping subdomains to tokens")
	}
	for subdomain, token := range g.agentTokens {
		if !validSubdomain.MatchString(subdomain) || len(token) < 32 {
			log.Fatal("AGENT_TOKENS requires valid subdomains and tokens of at least 32 characters")
		}
	}
	if value := os.Getenv("REQUEST_TIMEOUT_SECONDS"); value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 1 || seconds > 300 {
			log.Fatal("REQUEST_TIMEOUT_SECONDS must be between 1 and 300")
		}
		g.timeout = time.Duration(seconds) * time.Second
	}
	for _, value := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		if strings.TrimSpace(value) == "" {
			continue
		}
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			log.Fatal("invalid TRUSTED_PROXY_CIDRS")
		}
		g.trustedProxies = append(g.trustedProxies, network)
	}
}
func (g *Gateway) controlHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			if !g.secureControlRequest(r) {
				http.Error(w, "remote control access requires HTTPS", http.StatusUpgradeRequired)
				return
			}
			release, ok := g.clients.admit("control:"+g.clientAddress(r), time.Now(), false)
			if !ok {
				w.Header().Set("Retry-After", "1")
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			defer release()
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if r.URL.Path == "/agent/connect" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		if _, password, ok := r.BasicAuth(); ok {
			token = password
		}
		if !tokenEqual(token, g.controlToken) {
			w.Header().Set("WWW-Authenticate", `Basic realm="Tunnel administrator", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+r.Host && origin != "http://"+r.Host {
				http.Error(w, "invalid origin", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
func redactHeaders(headers map[string][]string) map[string][]string {
	result := cloneHeaderMap(headers)
	for key := range result {
		switch strings.ToLower(key) {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key":
			result[key] = []string{"[REDACTED]"}
		}
	}
	return result
}
func trafficSize(record *TrafficRecord) int64 {
	size := len(record.RequestBody) + len(record.ResponseBody) + len(record.Path) + len(record.RawQuery)
	for _, headers := range []map[string][]string{record.RequestHeaders, record.ResponseHeaders} {
		for key, values := range headers {
			size += len(key)
			for _, value := range values {
				size += len(value)
			}
		}
	}
	return int64(size)
}

func proxyErrorStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	if errors.Is(err, errTunnelBusy) {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadGateway
}
func (s *AgentSession) redisLease() string {
	return fmt.Sprintf("%s:%d", s.agentID, s.connectedAt.UnixNano())
}
