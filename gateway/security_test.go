package main

import (
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bar-meet-tunnel/bar-meet-tunnel/internal/protocol"
)

type observedBody struct{ read bool }

func (b *observedBody) Read([]byte) (int, error) { b.read = true; return 0, io.EOF }
func (*observedBody) Close() error               { return nil }

func TestTunnelAdmissionPrecedesBodyAndGlobalSlot(t *testing.T) {
	g := testGateway()
	session := &AgentSession{slots: make(chan struct{}, 1)}
	session.slots <- struct{}{}
	g.sessionsBySubdomain["demo"] = session
	req := httptest.NewRequest("POST", "http://demo.tunnel.com/upload", nil)
	body := &observedBody{}
	req.Body = body
	w := httptest.NewRecorder()
	g.handlePublicRequest(w, req)
	if w.Code != 503 || body.read || len(g.slots) != 0 {
		t.Fatalf("admission failed: status %d, body read %v, slots %d", w.Code, body.read, len(g.slots))
	}
}
func TestAdmissionRollback(t *testing.T) {
	g := testGateway()
	g.slots = make(chan struct{}, 1)
	g.slots <- struct{}{}
	session := &AgentSession{slots: make(chan struct{}, 1)}
	if _, err := g.admit(session); !errors.Is(err, errTunnelBusy) {
		t.Fatal(err)
	}
	if len(session.slots) != 0 {
		t.Fatal("tunnel slot leaked")
	}
}
func TestClientLimiter(t *testing.T) {
	var limiter clientLimiter
	now := time.Now()
	releases := make([]func(), 0, clientConcurrency)
	for i := 0; i < clientConcurrency; i++ {
		release, ok := limiter.admit("one", now, true)
		if !ok {
			t.Fatal("early rejection")
		}
		releases = append(releases, release)
	}
	if _, ok := limiter.admit("one", now, true); ok {
		t.Fatal("concurrency exceeded")
	}
	for _, release := range releases {
		release()
	}
	for i := clientConcurrency; i < clientBurst; i++ {
		release, ok := limiter.admit("one", now, true)
		if !ok {
			t.Fatal("burst rejected")
		}
		release()
	}
	if _, ok := limiter.admit("one", now, true); ok {
		t.Fatal("rate limit exceeded")
	}
	release, ok := limiter.admit("one", now.Add(time.Second), true)
	if !ok {
		t.Fatal("tokens not replenished")
	}
	release()
}
func TestClientLimiterBoundsAndExpiry(t *testing.T) {
	var limiter clientLimiter
	now := time.Now()
	for i := 0; i < maxClientBuckets; i++ {
		if _, ok := limiter.admit(string(rune(i)), now, false); !ok {
			t.Fatal("early rejection")
		}
	}
	if _, ok := limiter.admit("new", now, false); ok {
		t.Fatal("unbounded limiter map")
	}
	if _, ok := limiter.admit("new", now.Add(clientBucketTTL), false); !ok {
		t.Fatal("expired buckets not reclaimed")
	}
}
func TestCaptureModesAndExpiry(t *testing.T) {
	for _, mode := range []string{"off", "metadata", "full"} {
		t.Run(mode, func(t *testing.T) {
			g := testGateway()
			g.captureMode = mode
			g.redactExtra = []string{"X-Secret"}
			snapshot := RequestSnapshot{RawQuery: "token=secret", Headers: map[string][]string{"Authorization": {"secret"}, "X-Secret": {"secret"}}, Body: []byte("private")}
			response := &protocol.ProxyResponse{ID: "req-1", Body: []byte("private")}
			record := g.recordTraffic(&AgentSession{}, snapshot, response, time.Now())
			if mode == "off" {
				if len(g.trafficByID) != 0 {
					t.Fatal("off retained traffic")
				}
				return
			}
			if mode == "metadata" && (len(record.RequestBody) != 0 || len(record.ResponseBody) != 0 || record.RawQuery != "" || record.Replayable || record.RequestHeaders["X-Secret"][0] != "[REDACTED]") {
				t.Fatal("metadata retained sensitive payload")
			}
			if mode == "full" && (!record.Replayable || string(record.RequestBody) != "private" || record.RequestHeaders["X-Secret"][0] != "secret") {
				t.Fatal("full capture corrupted replay data")
			}
			if g.trafficView(record).RequestHeaders["X-Secret"][0] != "[REDACTED]" {
				t.Fatal("custom header exposed")
			}
			g.expireTraffic(record.ExpiresAt)
			if len(g.trafficByID) != 0 || len(g.trafficOrder) != 0 || g.historyBytes != 0 {
				t.Fatal("expired traffic retained")
			}
		})
	}
}
func TestMetadataReplayRejected(t *testing.T) {
	g := testGateway()
	g.captureMode = "metadata"
	g.recordTraffic(&AgentSession{subdomain: "demo"}, RequestSnapshot{}, &protocol.ProxyResponse{ID: "req-1"}, time.Now())
	w := httptest.NewRecorder()
	g.handleAPIRequestByID(w, httptest.NewRequest("POST", "/api/requests/req-1/replay", nil))
	if w.Code != 409 {
		t.Fatalf("metadata replay status %d", w.Code)
	}
}

func TestRemoteControlRequiresTrustedTLS(t *testing.T) {
	g := testGateway()
	req := httptest.NewRequest("GET", "http://control.example/ui", nil)
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("X-Forwarded-Proto", "https")
	if g.secureControlRequest(req) {
		t.Fatal("untrusted TLS claim accepted")
	}
	_, network, _ := net.ParseCIDR("192.0.2.1/32")
	g.trustedProxies = []*net.IPNet{network}
	if !g.secureControlRequest(req) {
		t.Fatal("trusted TLS terminator rejected")
	}
	req.Header.Del("X-Forwarded-Proto")
	if g.secureControlRequest(req) {
		t.Fatal("plaintext remote access accepted")
	}
	req.RemoteAddr = "127.0.0.1:1234"
	if !g.secureControlRequest(req) {
		t.Fatal("loopback development rejected")
	}
}
