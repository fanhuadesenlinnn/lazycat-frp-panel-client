package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ProbeResult is raw, timestamped evidence about the master endpoint.
//
// It is deliberately not called "online": reaching the master's HTTP port says
// nothing about whether this client registered successfully.
type ProbeResult struct {
	Target     string    `json:"target"`
	At         time.Time `json:"at"`
	TCP        string    `json:"tcp"`
	HTTP       string    `json:"http"`
	HTTPStatus int       `json:"http_status"`
	LatencyMS  int64     `json:"latency_ms"`
	Error      string    `json:"error"`
}

// Probe periodically checks whether the configured master endpoint answers.
type Probe struct {
	mu     sync.RWMutex
	target string
	last   ProbeResult
	client *http.Client
}

// NewProbe creates a Probe for the given API URL.
func NewProbe(apiURL string) *Probe {
	return &Probe{
		target: strings.TrimSpace(apiURL),
		client: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// SetTarget updates the probed endpoint.
func (p *Probe) SetTarget(apiURL string) {
	p.mu.Lock()
	p.target = strings.TrimSpace(apiURL)
	p.last = ProbeResult{}
	p.mu.Unlock()
}

// Target returns the current endpoint.
func (p *Probe) Target() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.target
}

// Last returns the most recent result.
func (p *Probe) Last() ProbeResult {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.last
}

// Run performs one probe round and stores the result.
func (p *Probe) Run(ctx context.Context) ProbeResult {
	p.mu.RLock()
	target := p.target
	p.mu.RUnlock()

	res := ProbeResult{Target: target, At: time.Now(), TCP: "skipped", HTTP: "skipped"}
	if target == "" {
		res.Error = "尚未配置 API URL"
		p.store(res)
		return res
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		res.Error = "API URL 无法解析"
		p.store(res)
		return res
	}
	host := u.Host
	if u.Port() == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			host = net.JoinHostPort(u.Hostname(), "443")
		default:
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}

	start := time.Now()
	conn, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		res.TCP = "fail"
		res.Error = err.Error()
		res.LatencyMS = time.Since(start).Milliseconds()
		p.store(res)
		return res
	}
	_ = conn.Close()
	res.TCP = "ok"
	res.LatencyMS = time.Since(start).Milliseconds()

	if u.Scheme == "http" || u.Scheme == "https" {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
		if err != nil {
			res.HTTP = "fail"
			res.Error = err.Error()
			p.store(res)
			return res
		}
		req.Header.Set("User-Agent", "lzc-frpp-client-probe")
		resp, err := p.client.Do(req)
		if err != nil {
			res.HTTP = "fail"
			res.Error = err.Error()
		} else {
			_, _ = io.CopyN(io.Discard, resp.Body, 4096)
			_ = resp.Body.Close()
			res.HTTP = "ok"
			res.HTTPStatus = resp.StatusCode
		}
	}
	res.LatencyMS = time.Since(start).Milliseconds()
	p.store(res)
	return res
}

func (p *Probe) store(res ProbeResult) {
	p.mu.Lock()
	// A configuration change may have retargeted the probe while this round was
	// in flight; a stale result must not overwrite the new target's state.
	if p.target != res.Target {
		p.mu.Unlock()
		return
	}
	p.last = res
	p.mu.Unlock()
}

// Loop probes every interval until the context is cancelled.
func (p *Probe) Loop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	p.Run(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Run(ctx)
		}
	}
}
