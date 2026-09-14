package main

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	clientBurst             = 60
	clientRequestsPerSecond = 5
	clientConcurrency       = 4
	maxClientBuckets        = 4096
	clientBucketTTL         = 10 * time.Minute
)

type clientBucket struct {
	tokens  float64
	updated time.Time
	active  int
}

// The map is bounded as well as the requests, so spoofed/rotating IPs cannot
// grow limiter state indefinitely. A full map rejects unknown clients.
type clientLimiter struct {
	mu      sync.Mutex
	buckets map[string]*clientBucket
}

func (l *clientLimiter) admit(key string, now time.Time, concurrent bool) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = make(map[string]*clientBucket)
	}
	bucket := l.buckets[key]
	if bucket == nil {
		if len(l.buckets) >= maxClientBuckets {
			for key, entry := range l.buckets {
				if entry.active == 0 && now.Sub(entry.updated) >= clientBucketTTL {
					delete(l.buckets, key)
				}
			}
			if len(l.buckets) >= maxClientBuckets {
				return nil, false
			}
		}
		bucket = &clientBucket{tokens: clientBurst, updated: now}
		l.buckets[key] = bucket
	}
	elapsed := now.Sub(bucket.updated).Seconds()
	if elapsed > 0 {
		bucket.tokens = min(clientBurst, bucket.tokens+elapsed*clientRequestsPerSecond)
		bucket.updated = now
	}
	if bucket.tokens < 1 || concurrent && bucket.active >= clientConcurrency {
		return nil, false
	}
	bucket.tokens--
	if concurrent {
		bucket.active++
	}
	return func() {
		if concurrent {
			l.mu.Lock()
			bucket.active--
			l.mu.Unlock()
		}
	}, true
}

// Reserve tunnel capacity before global capacity, and before reading a body.
func (g *Gateway) admit(session *AgentSession) (func(), error) {
	select {
	case session.slots <- struct{}{}:
	default:
		return nil, errTunnelBusy
	}
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots; <-session.slots }, nil
	default:
		<-session.slots
		return nil, errTunnelBusy
	}
}

func (g *Gateway) redactHeaders(headers map[string][]string) map[string][]string {
	result := redactHeaders(headers)
	for key := range result {
		for _, name := range g.redactExtra {
			if http.CanonicalHeaderKey(key) == http.CanonicalHeaderKey(name) {
				result[key] = []string{"[REDACTED]"}
			}
		}
	}
	return result
}

func (g *Gateway) trafficView(record *TrafficRecord) TrafficRecordView {
	view := record.view()
	view.RequestHeaders = g.redactHeaders(record.RequestHeaders)
	view.ResponseHeaders = g.redactHeaders(record.ResponseHeaders)
	return view
}

func (g *Gateway) expireTraffic(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	retained := g.trafficOrder[:0]
	for _, id := range g.trafficOrder {
		record := g.trafficByID[id]
		if record == nil {
			continue
		}
		if !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt) {
			g.historyBytes -= trafficSize(record)
			delete(g.trafficByID, id)
		} else {
			retained = append(retained, id)
		}
	}
	clear(g.trafficOrder[len(retained):])
	g.trafficOrder = retained
}

func (g *Gateway) expireTrafficLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			g.expireTraffic(now)
		case <-ctx.Done():
			return
		}
	}
}

// Only a known TLS terminator may vouch for encryption on the previous hop.
func (g *Gateway) secureControlRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip.IsLoopback() {
		return true
	}
	if r.Header.Get("X-Forwarded-Proto") != "https" {
		return false
	}
	for _, network := range g.trustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
