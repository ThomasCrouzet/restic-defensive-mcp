package testrepo

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProxyRule injects a bounded failure into requests routed through a test
// proxy. Empty Methods matches every method; PathContains matches a substring
// of the request path. Delay runs before the other actions.
type ProxyRule struct {
	Methods      []string
	PathContains string
	Delay        time.Duration
	Stall        bool
	Interrupt    bool
	Status       int
}

// Proxy fronts a disposable service with plain HTTP and a self-signed TLS
// listener. Tests switch rules without restarting the service or the server.
type Proxy struct {
	URL      string
	TLSURL   string
	upstream *url.URL
	reverse  *httputil.ReverseProxy
	servers  []*httptest.Server
	rules    atomic.Pointer[[]ProxyRule]
	counters sync.Map
	closed   chan struct{}
	close    sync.Once
}

// StartProxy starts pass-through HTTP and TLS listeners in front of upstream.
// upstream is a plain http URL such as http://127.0.0.1:1234.
func StartProxy(upstream string) (*Proxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, fmt.Errorf("parse proxy upstream: %w", err)
	}
	if target.Scheme != "http" || target.Host == "" {
		return nil, fmt.Errorf("proxy upstream must be a plain http URL")
	}
	proxy := &Proxy{
		upstream: target,
		reverse:  newReverseProxy(target),
		closed:   make(chan struct{}),
	}
	empty := []ProxyRule{}
	proxy.rules.Store(&empty)

	plain := httptest.NewUnstartedServer(proxy)
	plain.Config.ErrorLog = log.New(io.Discard, "", 0)
	plain.Start()
	tlsServer := httptest.NewUnstartedServer(proxy)
	tlsServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	tlsServer.StartTLS()
	proxy.servers = []*httptest.Server{plain, tlsServer}
	proxy.URL = plain.URL
	proxy.TLSURL = tlsServer.URL
	return proxy, nil
}

// SetRules replaces the active rules. With no rules the proxy is transparent.
func (p *Proxy) SetRules(rules ...ProxyRule) {
	copied := append([]ProxyRule(nil), rules...)
	p.rules.Store(&copied)
}

// Close shuts down both listeners and unblocks stalled handlers.
func (p *Proxy) Close() {
	p.close.Do(func() {
		close(p.closed)
		for _, server := range p.servers {
			server.Close()
		}
	})
}

// RequestCount reports how many requests contained pathContains. Tests use it
// to prove that a stalled command reached the backend data path.
func (p *Proxy) RequestCount(pathContains string) int64 {
	value, ok := p.counters.Load(pathContains)
	if !ok {
		return 0
	}
	return value.(*atomic.Int64).Load()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/data/") {
		counter, _ := p.counters.LoadOrStore("/data/", &atomic.Int64{})
		counter.(*atomic.Int64).Add(1)
	}
	for _, rule := range *p.rules.Load() {
		if !ruleMatches(rule, r) {
			continue
		}
		p.apply(rule, w, r)
		return
	}
	p.reverse.ServeHTTP(w, r)
}

func ruleMatches(rule ProxyRule, r *http.Request) bool {
	if rule.PathContains != "" && !strings.Contains(r.URL.Path, rule.PathContains) {
		return false
	}
	if len(rule.Methods) == 0 {
		return true
	}
	for _, method := range rule.Methods {
		if strings.EqualFold(method, r.Method) {
			return true
		}
	}
	return false
}

func (p *Proxy) apply(rule ProxyRule, w http.ResponseWriter, r *http.Request) {
	if rule.Delay > 0 {
		timer := time.NewTimer(rule.Delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		case <-p.closed:
			return
		}
	}
	if rule.Stall {
		select {
		case <-r.Context().Done():
		case <-p.closed:
		}
		return
	}
	if rule.Interrupt {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "interrupt unsupported", http.StatusInternalServerError)
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		_, _ = io.WriteString(connection, "HTTP/1.1 200 OK\r\nContent-Length: 1048576\r\n\r\ninterrupted")
		_ = connection.Close()
		return
	}
	if rule.Status != 0 {
		w.WriteHeader(rule.Status)
		_, _ = io.WriteString(w, "synthetic backend failure")
		return
	}
	p.reverse.ServeHTTP(w, r)
}

func newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	// NewSingleHostReverseProxy rewrites URL scheme, host, and path but leaves
	// the caller's Host header intact. S3 SigV4 signs that header, so the
	// disposable gateway validates the same canonical request.
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorLog = log.New(io.Discard, "", 0)
	return proxy
}
