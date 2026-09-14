package main

import (
	"context"
	"fmt"
	"github.com/bar-meet-tunnel/bar-meet-tunnel/internal/protocol"
	"github.com/gorilla/websocket"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtractSubdomain(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
		ok   bool
	}{
		{name: "valid host", host: "demo.tunnel.com", want: "demo", ok: true},
		{name: "valid host with port", host: "demo.tunnel.com:8080", want: "demo", ok: true},
		{name: "invalid host", host: "localhost", want: "", ok: false},
		{name: "invalid subdomain", host: "Bad!.tunnel.com", want: "", ok: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := extractSubdomain(test.host)
			if got != test.want || ok != test.ok {
				t.Fatalf("extractSubdomain(%q) = (%q, %v), want (%q, %v)", test.host, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestPreviewBody(t *testing.T) {
	textView := previewBody([]byte("hello"))
	if textView.Encoding != "text" || textView.Content != "hello" || textView.Truncated {
		t.Fatalf("unexpected text preview: %+v", textView)
	}

	binaryView := previewBody([]byte{0xff, 0x00, 0x01})
	if binaryView.Encoding != "base64" || binaryView.Content == "" {
		t.Fatalf("unexpected binary preview: %+v", binaryView)
	}
}

func testGateway() *Gateway {
	return &Gateway{captureMode: "full", captureTTL: 15 * time.Minute, controlToken: "admin-secret", agentTokens: map[string]string{"demo": "agent-secret"}, timeout: time.Second, slots: make(chan struct{}, 32), sessionsBySubdomain: make(map[string]*AgentSession), sessionsByAgentID: make(map[string]*AgentSession), trafficByID: make(map[string]*TrafficRecord)}
}
func TestControlAuthenticationAndRedaction(t *testing.T) {
	g := testGateway()
	g.trafficByID["req-1"] = &TrafficRecord{ID: "req-1", RequestHeaders: map[string][]string{"Authorization": {"secret"}}}
	handler := g.controlHandler(http.HandlerFunc(g.handleAPIRequestByID))
	for _, token := range []string{"", "wrong", "admin-secret"} {
		req := httptest.NewRequest("GET", "/api/requests/req-1", nil)
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if token != "admin-secret" && w.Code != 401 {
			t.Fatalf("unauthorized status: %d", w.Code)
		}
		if token == "admin-secret" && (w.Code != 200 || strings.Contains(w.Body.String(), "secret") || !strings.Contains(w.Body.String(), "REDACTED")) {
			t.Fatalf("unsafe response: %s", w.Body.String())
		}
	}
	if g.trafficByID["req-1"].RequestHeaders["Authorization"][0] != "secret" {
		t.Fatal("redaction changed replay data")
	}
	req := httptest.NewRequest("POST", "http://control.example/api/requests/req-1/replay", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.SetBasicAuth("admin", "admin-secret")
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("cross-origin replay allowed")
	}
}
func TestTrustedClientAddress(t *testing.T) {
	g := testGateway()
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.5")
	if got := g.clientAddress(req); got != "192.0.2.1" {
		t.Fatal(got)
	}
	_, network, _ := net.ParseCIDR("192.0.2.0/24")
	g.trustedProxies = []*net.IPNet{network}
	if got := g.clientAddress(req); got != "203.0.113.5" {
		t.Fatal(got)
	}
}
func TestHistoryByteBudget(t *testing.T) {
	g := testGateway()
	session := &AgentSession{subdomain: "demo"}
	body := make([]byte, 10<<20)
	for i := 0; i < 8; i++ {
		g.recordTraffic(session, RequestSnapshot{Body: body}, &protocol.ProxyResponse{ID: fmt.Sprint(i), Body: body}, time.Now())
	}
	if g.historyBytes > maxHistoryBytes || len(g.trafficOrder) != 3 {
		t.Fatalf("unbounded history: %d bytes, %d records", g.historyBytes, len(g.trafficOrder))
	}
}
func TestAgentAuthorizationForwardingReplayAndDisconnect(t *testing.T) {
	g := testGateway()
	server := httptest.NewServer(http.HandlerFunc(g.handleAgentConnect))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "?subdomain=demo"
	if conn, resp, err := websocket.DefaultDialer.Dial(endpoint, nil); err == nil {
		conn.Close()
		t.Fatal("unauthenticated connection accepted")
	} else if resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
	headers := http.Header{"Authorization": {"Bearer agent-secret"}}
	conn, _, err := websocket.DefaultDialer.Dial(endpoint, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.WriteJSON(protocol.Message{Type: protocol.MessageTypeHello, Hello: &protocol.Hello{AgentID: "agent", Subdomain: "demo"}})
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := g.lookupSession("demo"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("registration timed out")
		}
		time.Sleep(time.Millisecond)
	}
	duplicate, _, err := websocket.DefaultDialer.Dial(endpoint, headers)
	if err != nil {
		t.Fatal(err)
	}
	defer duplicate.Close()
	duplicate.WriteJSON(protocol.Message{Type: protocol.MessageTypeHello, Hello: &protocol.Hello{AgentID: "other", Subdomain: "demo"}})
	duplicate.SetReadDeadline(time.Now().Add(time.Second))
	var message protocol.Message
	if err := duplicate.ReadJSON(&message); err == nil {
		t.Fatal("duplicate accepted")
	}
	errors := make(chan error, 1)
	go func() {
		for i := 0; i < 2; i++ {
			var msg protocol.Message
			if err := conn.ReadJSON(&msg); err != nil {
				errors <- err
				return
			}
			if msg.Request.Path != "/a%2Fb" {
				errors <- fmt.Errorf("changed path: %s", msg.Request.Path)
				return
			}
			if i == 1 && msg.Request.ReplayOf == "" {
				errors <- fmt.Errorf("missing replay ID")
				return
			}
			if err := conn.WriteJSON(protocol.Message{Type: protocol.MessageTypeProxyResponse, Response: &protocol.ProxyResponse{ID: msg.Request.ID, StatusCode: 201, Body: []byte("ok")}}); err != nil {
				errors <- err
				return
			}
		}
		errors <- nil
	}()
	w := httptest.NewRecorder()
	g.handlePublicRequest(w, httptest.NewRequest("POST", "http://demo.tunnel.com/a%2Fb", strings.NewReader("payload")))
	if w.Code != 201 || w.Body.String() != "ok" {
		t.Fatalf("forwarding: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	g.handleAPIRequestByID(w, httptest.NewRequest("POST", "/api/requests/req-1/replay", nil))
	if w.Code != 200 {
		t.Fatalf("replay: %d %s", w.Code, w.Body.String())
	}
	if err := <-errors; err != nil {
		t.Fatal(err)
	}

	session, _ := g.lookupSession("demo")
	session.slots = make(chan struct{}, 1)
	session.slots <- struct{}{}
	if _, _, err := g.dispatchRequest(context.Background(), session, RequestSnapshot{}); err != errTunnelBusy {
		t.Fatalf("expected busy, got %v", err)
	}
	<-session.slots
	cancelCtx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		_, _, err := g.dispatchRequest(cancelCtx, session, RequestSnapshot{Method: "GET", Path: "/cancel"})
		finished <- err
	}()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	var request protocol.Message
	if err := conn.ReadJSON(&request); err != nil {
		t.Fatal(err)
	}
	cancel()
	var canceled protocol.Message
	if err := conn.ReadJSON(&canceled); err != nil {
		t.Fatal(err)
	}
	if canceled.Type != protocol.MessageTypeCancel || canceled.CancelID != request.Request.ID {
		t.Fatalf("unexpected cancel: %+v", canceled)
	}
	if err := <-finished; err != context.Canceled {
		t.Fatalf("cancellation error: %v", err)
	}
	session.pendingMu.Lock()
	pending := len(session.pending)
	session.pendingMu.Unlock()
	if pending != 0 {
		t.Fatal("pending request leaked")
	}
	conn.Close()
	deadline = time.Now().Add(time.Second)
	for {
		if _, ok := g.lookupSession("demo"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnect not removed")
		}
		time.Sleep(time.Millisecond)
	}
}
