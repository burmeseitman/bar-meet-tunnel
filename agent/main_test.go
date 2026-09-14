package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTargetPreservesEscapedPath(t *testing.T) {
	a := &Agent{LocalHost: "http://localhost:8080"}
	for _, path := range []string{"/files/a%2Fb", "/a%20b", "/a/../b", "//other.example/path"} {
		got, err := a.buildTargetURL(path, "x=1%202")
		if err != nil || got != "http://localhost:8080"+path+"?x=1%202" {
			t.Fatalf("path %s: %s, %v", path, got, err)
		}
	}
}
func TestClientPreservesRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/destination", 302) }))
	defer server.Close()
	a := newAgentFromEnv()
	resp, err := a.Client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/destination" {
		t.Fatal("redirect not preserved")
	}
}
func TestClientCancellation(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done(); close(stopped) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	done := make(chan error, 1)
	go func() {
		resp, err := newAgentFromEnv().Client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	<-started
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("upstream not canceled")
	}
	if <-done == nil {
		t.Fatal("expected canceled request error")
	}
}
func TestBodyAndConnectionHeaderLimits(t *testing.T) {
	if _, err := readBody(io.NopCloser(strings.NewReader("12345")), 4); err == nil {
		t.Fatal("oversized body accepted")
	}
	headers := filterHeaders(http.Header{"Connection": {"X-Private"}, "X-Private": {"secret"}, "Content-Type": {"text/plain"}})
	if len(headers) != 1 || headers["Content-Type"][0] != "text/plain" {
		t.Fatalf("unexpected headers: %v", headers)
	}
}

func TestGatewayRequiresRemoteTLS(t *testing.T) {
	for _, raw := range []string{"ws://example.com/agent/connect", "ws://192.168.1.2:9000/agent/connect", "http://localhost/agent/connect", "wss://user:secret@example.com/agent/connect", "ws://localhost.evil.test/agent/connect"} {
		if _, err := validateGatewayURL(raw); err == nil {
			t.Errorf("unsafe URL accepted: %s", raw)
		}
	}
	for _, raw := range []string{"ws://localhost:9000/agent/connect", "ws://127.0.0.1:9000/agent/connect", "ws://[::1]:9000/agent/connect", "wss://example.com/agent/connect"} {
		if _, err := validateGatewayURL(raw); err != nil {
			t.Errorf("valid URL rejected: %s: %v", raw, err)
		}
	}
}
