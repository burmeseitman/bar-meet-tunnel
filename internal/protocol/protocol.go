package protocol

import "time"

const (
	MessageTypeHello         = "hello"
	MessageTypeProxyRequest  = "proxy_request"
	MessageTypeProxyResponse = "proxy_response"
)

type Message struct {
	Type     string         `json:"type"`
	Hello    *Hello         `json:"hello,omitempty"`
	Request  *ProxyRequest  `json:"request,omitempty"`
	Response *ProxyResponse `json:"response,omitempty"`
}

type Hello struct {
	AgentID   string `json:"agent_id"`
	Subdomain string `json:"subdomain"`
	LocalHost string `json:"local_host"`
}

type ProxyRequest struct {
	ID         string              `json:"id"`
	Method     string              `json:"method"`
	Path       string              `json:"path"`
	RawQuery   string              `json:"raw_query,omitempty"`
	Host       string              `json:"host"`
	RemoteAddr string              `json:"remote_addr,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       []byte              `json:"body,omitempty"`
	ReplayOf   string              `json:"replay_of,omitempty"`
	StartedAt  time.Time           `json:"started_at"`
}

type ProxyResponse struct {
	ID          string              `json:"id"`
	StatusCode  int                 `json:"status_code"`
	Headers     map[string][]string `json:"headers,omitempty"`
	Body        []byte              `json:"body,omitempty"`
	Error       string              `json:"error,omitempty"`
	DurationMS  int64               `json:"duration_ms"`
	CompletedAt time.Time           `json:"completed_at"`
}
