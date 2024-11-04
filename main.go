package main

import (
    "flag"
    "fmt"
    "bytes"
    "crypto/tls"
    "io/ioutil"
    "net"
    "net/http"
    "os"
    "syscall"
    "os/signal"
    "sync"
    "time"
    "strings"
    "io"

    "gopkg.in/yaml.v2"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"

    "github.com/jessevdk/go-flags"
    "k8s.io/klog/v2"
)

type Opts struct {
    QueueSize      int    `long:"queue_size" short:"q" env:"QUEUE_SIZE" description:"buffer queue size/num of buffered requests" default:"100"`
    WorkerCount    int    `long:"worker_count" short:"w" env:"WORKER_COUNT" description:"count of worker goroutines to spawn" default:"5"`
    ListenAddress  string `long:"listen" short:"l" env:"LISTEN_ADDRESS" description:"listen bind address" default:":8080"`
    MetricsAddress string `long:"metrics_address" short:"m" env:"METRICS_ADDRESS" description:"metrics bind address" default:":9091"`
    ConfigPath     string `long:"config_path" short:"c" env:"CONFIG_PATH" description:"path to backend map file in yaml format" default:"/etc/backends.yaml"`
    LogLevel       int    `long:"loglevel" short:"v" env:"LOG_LEVEL" description:"log verbosity level for klog" default:"2"`
}

// BackendConfig represents the configuration for each backend
type BackendConfig struct {
    Backend string  `yaml:"backend"`
    Retries int     `yaml:"retries"`
    Delay   float32 `yaml:"delay`
    Timeout float32 `yaml:"timeout"` // Timeout in seconds
}

// Config represents the YAML structure mapping front-facing hosts to their backends
type Config map[string][]BackendConfig

// ProxyServer handles the proxying logic
type ProxyServer struct {
    configPath  string
    config    sync.Map         // sync.Map for thread-safe access to the configuration
    clients   sync.Map
    queue     chan *ForwardedRequest // Capped channel acting as the request queue
    workerCount int

    // Prometheus metrics
    totalRequests     *prometheus.CounterVec
    totalForwarded    *prometheus.CounterVec
    totalRetries      *prometheus.CounterVec
    totalFailed       *prometheus.CounterVec
    totalDropped      prometheus.Counter
    totalFailedBodyRead prometheus.Counter
    usedQueueLength     prometheus.Gauge
}


type ForwardedRequest struct {
    Req  *http.Request
    Body []byte
}

// NewProxyServer initializes a new ProxyServer with Prometheus metrics and worker goroutines
func NewProxyServer(configPath string, queueSize, workerCount int) *ProxyServer {
    ps := &ProxyServer{
        configPath:  configPath,
        queue:     make(chan *ForwardedRequest, queueSize), // Capped channel
        workerCount: workerCount,
        // Initialize Prometheus metrics
        totalRequests: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "pxfd_http_proxy_requests_total",
                Help: "Total number of incoming requests",
            },
            []string{"host"},
        ),
        totalForwarded: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "pxfd_http_proxy_forwarded_total",
                Help: "Total number of successfully forwarded requests",
            },
            []string{"host", "backend"},
        ),
        totalRetries: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "pxfd_http_proxy_retries_total",
                Help: "Total number of retries",
            },
            []string{"host", "backend"},
        ),
        totalFailed: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "pxfd_http_proxy_failed_total",
                Help: "Total number of failed requests",
            },
            []string{"host"},
        ),
        totalDropped: prometheus.NewCounter(prometheus.CounterOpts{
            Name: "pxfd_http_proxy_dropped_total",
            Help: "Total number of dropped requests",
        }),
        totalFailedBodyRead: prometheus.NewCounter(prometheus.CounterOpts{
            Name: "pxfd_http_proxy_failed_body_read_total",
            Help: "Total number of requests with failed body reads",
        }),
        usedQueueLength: prometheus.NewGauge(prometheus.GaugeOpts{
            Name: "pxfd_http_proxy_queue_length",
            Help: "Current length of the request queue",
        }),
    }

    // Register Prometheus metrics
    prometheus.MustRegister(ps.totalRequests, ps.totalForwarded, ps.totalRetries, ps.totalFailed, ps.totalDropped, ps.totalFailedBodyRead, ps.usedQueueLength)

    ps.loadConfig()                 // Load config initially
    go ps.reloadConfigPeriodically()    // Start goroutine to reload config every 30 seconds
    go ps.updateQueueLengthPeriodically() // Start goroutine to update queue length metrics

    // Start worker goroutines
    for i := 0; i < ps.workerCount; i++ {
        go ps.worker(i)
    }

    signalCh := make(chan os.Signal)
    signal.Notify(signalCh, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
    go func() {
        for sig := range signalCh {
          switch sig {
            case os.Interrupt:
              klog.V(2).Infof("Got interrput signal")
            case syscall.SIGTERM:
              klog.V(2).Infof("Got SIGTERM signal")
              // deliver rest of queue to FINAL destination
            case syscall.SIGQUIT:
              klog.V(2).Infof("Got SIGQUIT signal")
              // deliver rest of queue to FINAL destination
            default:
              klog.V(2).Infof("Some signal received: %v", sig)
          }
          klog.V(1).Infof("Queue len is: %v", ps.getQueueLen())
       }
    }()

    return ps
}

// loadConfig loads the configuration from the YAML file
func (p *ProxyServer) getQueueLen() int {
    return len(p.queue)
}

// loadConfig loads the configuration from the YAML file
func (p *ProxyServer) loadConfig() error {
    data, err := ioutil.ReadFile(p.configPath)
    if err != nil {
        klog.Fatal("Error reading config file: %v", err)
    }

    var newConfig Config
    if err := yaml.Unmarshal(data, &newConfig); err != nil {
        klog.V(1).Infof("Error parsing config file: %v", err)
        return err
    }

    // Update sync.Map with new config
    for host, backends := range newConfig {
        p.config.Store(host, backends)
    }

    // Clear old hosts in sync.Map
    p.config.Range(func(key, value interface{}) bool {
        found := false
        for host, _ := range newConfig {
            if host == key {
                found = true
            }
        }
        if !found {
            klog.V(2).Infof("Deleting old host %v", key)
            p.config.Delete(key)
        }
        return true
    })
    klog.V(4).Infof("Configuration reloaded successfully")
    return nil
}

// reloadConfigPeriodically reloads the config from the file every 30 seconds
func (p *ProxyServer) reloadConfigPeriodically() {
    for {
        time.Sleep(30 * time.Second)
        if err := p.loadConfig(); err != nil {
            klog.V(1).Infof("Failed to reload config:", err)
        }
    }
}

// updateQueueLengthPeriodically updates the queue length every 10 seconds
func (p *ProxyServer) updateQueueLengthPeriodically() {
    for {
        p.usedQueueLength.Set(float64(len(p.queue))) // Update queue length gauge
        time.Sleep(10 * time.Second)
    }
}

// worker processes requests from the queue
func (p *ProxyServer) worker(id int) {
    klog.V(2).Infof("Worker %d started", id)
    for req := range p.queue {
        p.proxyRequest(req) // Process each request from the queue
    }
}

// getBackendsForHost returns the backend configurations for a given host
func (p *ProxyServer) getBackendsForHost(host string) ([]BackendConfig, bool) {
    if backends, found := p.config.Load(host); found {
        return backends.([]BackendConfig), true
    }
    return nil, false
}

func (p *ProxyServer) getClientForHost(hostBackend string, timeout float32) (*http.Client) {
    timeoutStr := fmt.Sprintf("%f", timeout)
    hostBackend = hostBackend+timeoutStr

    if client, found := p.clients.Load(hostBackend); found {
        return client.(*http.Client)
    }
    client := &http.Client{
        //The timeout includes connection time, any
        // redirects, and reading the response body. The timer remains
        // running after Get, Head, Post, or Do return and will
        // interrupt reading of the Response.Body.
        Timeout: (time.Duration(timeout) + 1) * time.Second,
        Transport: &http.Transport{
            MaxIdleConns:      100,
            IdleConnTimeout:     90 * time.Second,
            MaxIdleConnsPerHost: 10,
            TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // Skip certificate verification (not recommended in production)
            DialContext: (&net.Dialer{
                //    Timeout is the maximum amount of time a dial will wait for
                // a connect to complete.
                Timeout: time.Duration(timeout) * time.Second,
            }).DialContext,
        },
    }
    p.clients.Store(hostBackend, client)
    return client
}

// proxyRequest forwards the request to the backend with retries based on the configuration
func (p *ProxyServer) proxyRequest(r *ForwardedRequest) {
    if r == nil || r.Req == nil {
        klog.Error("Received a nil request, skipping processing")
        return
    }

    host := r.Req.Host
    host, _, _ = strings.Cut(host, ":") // strip port
    backends, found := p.getBackendsForHost(host)

    p.totalRequests.WithLabelValues(host).Inc()

    if !found || len(backends) == 0 {
        p.totalFailed.WithLabelValues(host).Inc() // Increment failed request counter
        klog.V(1).Infof("Error host: '%v' not found in config file, dropping request", host)
        return
    }

    var lastErr error
    for _, backend := range backends {
        client := p.getClientForHost(host+backend.Backend, backend.Timeout)
        if client == nil {
            klog.V(1).Infof("Client for backend %s is nil, skipping", backend.Backend)
            continue
        }

        for i := 0; i <= backend.Retries; i++ {
            req, err := http.NewRequest(r.Req.Method, backend.Backend+r.Req.URL.Path, bytes.NewReader(r.Body))
            if err != nil {
                klog.V(4).Infof("Message failed for host %s with error: %v", backend.Backend, err)
                lastErr = err
                continue
            }

            req.Header = r.Req.Header
            resp, err := client.Do(req)
            if err != nil {
                klog.V(1).Infof("Failed to process request to %s with error: %v", backend.Backend, err)
                lastErr = err
                p.totalRetries.WithLabelValues(host, backend.Backend).Inc()
                time.Sleep(time.Duration(backend.Delay) * time.Second)
                continue
            }

            // Read and discard response body
            if _, err := io.Copy(io.Discard, resp.Body); err != nil {
                klog.V(1).Infof("Failed to read response body from %v: %v", backend.Backend+r.Req.URL.Path, err)
            }
            resp.Body.Close()

            if resp.StatusCode < 400 {
                p.totalForwarded.WithLabelValues(host, backend.Backend).Inc()
                klog.V(4).Infof("Request success to %s", backend.Backend)
                return
            } else {
                klog.V(4).Infof("Request to %s failed on not acceptable status code %v", backend.Backend, resp.StatusCode)
                time.Sleep(time.Duration(backend.Delay) * time.Second)
                continue
            }
        }
    }

    p.totalFailed.WithLabelValues(host).Inc()
    klog.V(1).Infof("All backends failed for host %s: %v", host, lastErr)
}

// handleIncomingRequest queues incoming requests
func (p *ProxyServer) handleIncomingRequest(w http.ResponseWriter, r *http.Request) {
    // Increment failed body read counter if the body can't be read
    body, err := ioutil.ReadAll(r.Body)
    if err != nil {
        p.totalFailedBodyRead.Inc()
        http.Error(w, "Failed to read body", http.StatusBadRequest)
        return
    }
    r.Body.Close() // Close the original body

    // Try to add the request to the queue
    select {
    case p.queue <- &ForwardedRequest{Req: r, Body: body}:
        //log.Printf(w, "Request queued\n")
    default:
        p.totalDropped.Inc() // Increment dropped requests counter
        http.Error(w, "Queue is full", http.StatusServiceUnavailable)
    }
}

func getEnv(key string, defaultValue string) string {
    if value, exists := os.LookupEnv(key); exists {
        return value
    }
    return defaultValue
}

func main() {
     klog.InitFlags(nil)

    var conf Opts
    _, err := flags.Parse(&conf)
    if err != nil {
        klog.Fatalf("Error parsing flags: %v", err)
    }
    flag.Set("v", fmt.Sprintf("%d", conf.LogLevel))

    // Create a new proxy server with the path to the YAML config
    proxy := NewProxyServer(conf.ConfigPath, conf.QueueSize, conf.WorkerCount) // queue size = 100, worker count = 5

    // HTTP handler for incoming requests
    http.HandleFunc("/", proxy.handleIncomingRequest)

    // Start the Prometheus metrics server on a separate port
    go func() {
        metricsMux := http.NewServeMux()
        metricsMux.Handle("/metrics", promhttp.Handler())
        klog.V(1).Infof("Prometheus metrics server listening on %s", conf.MetricsAddress)
        err := http.ListenAndServe(conf.MetricsAddress, metricsMux)
        if err != nil {
            klog.Fatalf("Failed to start server: %v", err) // %v formats the error as a string
        }
    }()

    // Start the proxy server
    klog.V(1).Infof("Proxy server is listening on %s", conf.ListenAddress)
    err = http.ListenAndServe(conf.ListenAddress, nil)
    if err != nil {
        klog.Fatalf("Failed to start server: %v", err) // %v formats the error as a string
    }
}
