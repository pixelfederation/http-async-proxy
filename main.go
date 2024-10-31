package main

import (
    "fmt"
    "bytes"
    "crypto/tls"
    "io/ioutil"
    "log"
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
)

type Opts struct {
    QueueSize      int    `long:"queue_size" short:"q" env:"QUEUE_SIZE" description:"buffer queue size/num of buffered requests" default:"100"`
    WorkerCount    int    `long:"worker_count" short:"w" env:"WORKER_COUNT" description:"count of worker goroutines to spawn" default:"5"`
    ListenAddress  string `long:"listen" short:"l" env:"LISTEN_ADDRESS" description:"listen bind address" default:":8080"`
    MetricsAddress string `long:"metrics_address" short:"m" env:"METRICS_ADDRESS" description:"metrics bind address" default:":9091"`
    ConfigPath     string `long:"config_path" short:"c" env:"CONFIG_PATH" description:"path to backend map file in yaml format" default:"/etc/backends.yaml"`
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
    totalRequests     prometheus.Counter
    totalForwarded    prometheus.Counter
    totalRetries      prometheus.Counter
    totalFailed       prometheus.Counter
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
        totalRequests: prometheus.NewCounter(prometheus.CounterOpts{
            Name: "proxy_requests_total",
            Help: "Total number of incoming requests",
        }),
        totalForwarded: prometheus.NewCounter(prometheus.CounterOpts{
            Name: "proxy_forwarded_total",
            Help: "Total number of successfully forwarded requests",
        }),
        totalRetries: prometheus.NewCounter(prometheus.CounterOpts{
            Name: "proxy_retries_total",
            Help: "Total number of retries",
        }),
        totalFailed: prometheus.NewCounter(prometheus.CounterOpts{
            Name: "proxy_failed_total",
            Help: "Total number of failed requests",
        }),
        totalDropped: prometheus.NewCounter(prometheus.CounterOpts{
            Name: "proxy_dropped_total",
            Help: "Total number of dropped requests",
        }),
        totalFailedBodyRead: prometheus.NewCounter(prometheus.CounterOpts{
            Name: "proxy_failed_body_read_total",
            Help: "Total number of requests with failed body reads",
        }),
        usedQueueLength: prometheus.NewGauge(prometheus.GaugeOpts{
            Name: "proxy_queue_length",
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
              log.Printf("Got interrput signal")
            case syscall.SIGTERM:
              log.Printf("Got SIGTERM signal")
              // deliver rest of queue to FINAL destination
            case syscall.SIGQUIT:
              log.Printf("Got SIGQUIT signal")
              // deliver rest of queue to FINAL destination
            default:
              log.Printf("Some signal received: %v\n", sig)
          }
          log.Printf("Queue len is: %v\n", ps.getQueueLen())
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
        log.Fatal("Error reading config file: %v\n", err)
    }

    var newConfig Config
    if err := yaml.Unmarshal(data, &newConfig); err != nil {
        log.Printf("Error parsing config file: %v\n", err)
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
            log.Printf("Deleting old host %v\n", key)
            p.config.Delete(key)
        }
        return true
    })
    /* Printf("New map:")
    p.config.Range(func(key, value interface{}) bool {
        log.Printf("%v\n", key)
        log.Printf("%v\n", value)
        return true
    })
    */
    log.Println("Configuration reloaded successfully")
    return nil
}

// reloadConfigPeriodically reloads the config from the file every 30 seconds
func (p *ProxyServer) reloadConfigPeriodically() {
    for {
        time.Sleep(30 * time.Second)
        if err := p.loadConfig(); err != nil {
            log.Println("Failed to reload config:", err)
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
    log.Printf("Worker %d started\n", id)
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
    // Increment total requests counter
    p.totalRequests.Inc()

    host := r.Req.Host // The front-facing host
    host , _, _ = strings.Cut(host, ":") //strip port
    backends, found := p.getBackendsForHost(host)

    if !found || len(backends) == 0 {
        p.totalFailed.Inc() // Increment failed request counter
        log.Printf("Error host: '%v' not found in config file, droping request\n", host)
        return
    }

    var lastErr error
    for _, backend := range backends {
        client := p.getClientForHost(host + backend.Backend, backend.Timeout)
        for i := 0; i <= backend.Retries; i++ {
            req, err := http.NewRequest(r.Req.Method, backend.Backend+r.Req.URL.Path, bytes.NewReader(r.Body));
            if err != nil {
                lastErr = err
                continue
            }

            req.Header = r.Req.Header
            resp, err := client.Do(req)
            // Read the response body to ensure the connection can be reused
            if _, err := io.Copy(io.Discard, resp.Body); err != nil {
                log.Printf("Failed to read %v response body: %v\n", backend.Backend +r.Req.URL.Path, err)
            }

            resp.Body.Close()
            if err == nil && resp.StatusCode < 400 {
                // Successfully forwarded request
                p.totalForwarded.Inc()
                return
            }
            lastErr = err
            p.totalRetries.Inc()                         // Increment retries counter
            time.Sleep(time.Duration(backend.Delay) * time.Second) // Small delay before retrying
        }
    }

    // If we get here, all backends failed
    p.totalFailed.Inc() // Increment failed requests counter
    log.Printf("All backends failed for host %s: %v\n", host, lastErr)
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
    var conf Opts
    _, err := flags.Parse(&conf)
    if err != nil {
        log.Fatal(err)
    }

    // Create a new proxy server with the path to the YAML config
    proxy := NewProxyServer(conf.ConfigPath, conf.QueueSize, conf.WorkerCount) // queue size = 100, worker count = 5

    // HTTP handler for incoming requests
    http.HandleFunc("/", proxy.handleIncomingRequest)

    // Start the Prometheus metrics server on a separate port
    go func() {
        metricsMux := http.NewServeMux()
        metricsMux.Handle("/metrics", promhttp.Handler())
        log.Printf("Prometheus metrics server listening on %s\n", conf.MetricsAddress)
        log.Fatal(http.ListenAndServe(conf.MetricsAddress, metricsMux))
    }()

    // Start the proxy server
    log.Printf("Proxy server is listening on %s\n", conf.ListenAddress)
    log.Fatal(http.ListenAndServe(conf.ListenAddress, nil))
}
