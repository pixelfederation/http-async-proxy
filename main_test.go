package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v2"
)

// newTestProxy starts a proxy that forwards host "h" to backend
func newTestProxy(t *testing.T, backend BackendConfig) *ProxyServer {
	t.Helper()
	data, err := yaml.Marshal(Config{"h": {backend}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "backends.yaml")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return NewProxyServer(path, 100, 2, prometheus.NewRegistry())
}

func forward(p *ProxyServer) {
	req, _ := http.NewRequest("POST", "http://h/event", strings.NewReader("x"))
	p.proxyRequest(&ForwardedRequest{Req: req, Body: []byte("x")})
}

func TestRetryDelayIsNotTruncated(t *testing.T) {
	var mu sync.Mutex
	var hits []time.Time
	backend := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, time.Now())
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := newTestProxy(t, BackendConfig{Backend: backend, Retries: 2, Delay: 0.2, Timeout: 2})

	forward(p)

	if len(hits) != 3 {
		t.Fatalf("got %d attempts, want 3", len(hits))
	}
	for i := 1; i < len(hits); i++ {
		if gap := hits[i].Sub(hits[i-1]); gap < 200*time.Millisecond {
			t.Errorf("retry %d came %v after the previous attempt, want >= 200ms", i, gap)
		}
	}
}

func TestTimeoutIsNotTruncated(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	backend := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		<-release // don't answer while the test runs
	})
	p := newTestProxy(t, BackendConfig{Backend: backend, Timeout: 0.5})

	start := time.Now()
	forward(p)

	// timeout + 1s; truncating 0.5 to 0 gave 1s
	if took := time.Since(start); took < 1400*time.Millisecond || took > 1900*time.Millisecond {
		t.Errorf("request gave up after %v, want 1.5s", took)
	}
}

func TestShutdownDeliversQueuedRequests(t *testing.T) {
	var delivered atomic.Int64
	backend := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		delivered.Add(1)
	})
	p := newTestProxy(t, BackendConfig{Backend: backend, Timeout: 2})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(p.handleIncomingRequest)}
	go srv.Serve(ln)

	const sent = 20 // 2 workers x 50ms: most are still queued at shutdown
	for i := 0; i < sent; i++ {
		req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/event", strings.NewReader("x"))
		req.Host = "h"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	p.shutdown(srv)

	if got := delivered.Load(); got != sent {
		t.Errorf("delivered %d of %d requests before shutdown returned", got, sent)
	}
}

func newBackend(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}
