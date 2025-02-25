package app

import (
    "context"
    "crypto/tls"
    "crypto/x509"
    "encoding/json"
    "fmt"
    "io/ioutil"
    "net/http"
    "sync"
    "time"

    "github.com/example/db-monitoring-app/internal/db"
    "github.com/example/db-monitoring-app/internal/utils"
    "github.com/fsnotify/fsnotify"
    "github.com/gojek/heimdall/v7/hystrix"
    "github.com/juju/ratelimit"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "github.com/sirupsen/logrus"
    "gopkg.in/yaml.v3"
)

type Config struct {
    GlobalConfig struct {
        LogLevel               string `yaml:"log_level"`
        RetryConnInterval      int    `yaml:"retry_conn_interval"`
        DefaultTimeInterval    int    `yaml:"default_time_interval"`
        LogPath                string `yaml:"log_path"`
        Port                   int    `yaml:"port"`
        UseHTTPS               bool   `yaml:"use_https"`
        CertFile               string `yaml:"cert_file"`
        KeyFile                string `yaml:"key_file"`
        PrometheusMTLSEnabled  bool   `yaml:"prometheus_mtls_enabled"`
        PrometheusClientCACert string `yaml:"prometheus_client_ca_cert_file"`
        ShutdownTimeout        int    `yaml:"shutdown_timeout"`
        WorkerPoolSize         int    `yaml:"worker_pool_size"`
        EncryptionKey          string `yaml:"encryption_key"`
        RateLimitRequests      int    `yaml:"rate_limit_requests"`
        RateLimitBurst         int    `yaml:"rate_limit_burst"`
        CircuitBreakerConfig   struct {
            Timeout       int `yaml:"timeout"`
            MaxConcurrent int `yaml:"max_concurrent"`
            ErrorPercent  int `yaml:"error_percent"`
            SleepWindow   int `yaml:"sleep_window"`
        } `yaml:"circuit_breaker_config"`
    } `yaml:"global_config"`
    Queries     []Query     `yaml:"queries"`
    Connections []Connection `yaml:"connections"`
    BasicAuth   struct {
        Username string `yaml:"username"`
        Password string `yaml:"password"`
    } `yaml:"basic_auth"`
}

type Query struct {
    Name         string            `yaml:"name"`
    DBType       string            `yaml:"db_type"`
    RunsOn       []string          `yaml:"runs_on"`
    TimeInterval int               `yaml:"time_interval"`
    Query        string            `yaml:"query"`
    Gauges       []Gauge           `yaml:"gauges"`
    Timeout      int               `yaml:"timeout,omitempty"`
    Priority     int               `yaml:"priority,omitempty"`
}

type Gauge struct {
    Name        string            `yaml:"name"`
    Desc        string            `yaml:"desc"`
    Col         int               `yaml:"col"`
    ExtraLabels map[string]string `yaml:"extra_labels,omitempty"`
}

type Connection struct {
    DBHost        string            `yaml:"db_host"`
    DBName        string            `yaml:"db_name"`
    DBPort        int               `yaml:"db_port"`
    DBUser        string            `yaml:"db_user"`
    DBPasswd      string            `yaml:"db_passwd"`
    DBType        string            `yaml:"db_type"`
    TLSEnabled    bool              `yaml:"tls_enabled"`
    TLSCertFile   string            `yaml:"tls_cert_file"`
    TLSKeyFile    string            `yaml:"tls_key_file"`
    TLSCACertFile string            `yaml:"tls_ca_cert_file"`
    Tags          []string          `yaml:"tags"`
    ExtraLabels   map[string]string `yaml:"extra_labels"`
    MaxConns      int               `yaml:"max_conns,omitempty"`
    IdleTimeout   int               `yaml:"idle_timeout,omitempty"`
}

type Application struct {
    config         Config
    dbClients      map[string]*db.DBClient
    workerPool     chan QueryJob
    circuitBreakers map[string]*hystrix.Client
    dlq            *DeadLetterQueue
    shutdown       chan struct{}
    wg             sync.WaitGroup
    mu             sync.Mutex
    server         *http.Server
}

type QueryJob struct {
    Query      Query
    Connection Connection
    Context    context.Context
}

func NewApplication(configFile string) (*Application, error) {
    config, err := loadConfig(configFile)
    if err != nil {
        return nil, err
    }

    app := &Application{
        config:         config,
        dbClients:      make(map[string]*db.DBClient),
        workerPool:     make(chan QueryJob, config.GlobalConfig.WorkerPoolSize),
        circuitBreakers: make(map[string]*hystrix.Client),
        dlq:            NewDeadLetterQueue(config.GlobalConfig.LogPath),
        shutdown:       make(chan struct{}),
    }

    for _, conn := range config.Connections {
        client, err := db.NewDBClient(conn)
        if err != nil {
            logrus.Errorf("Failed to initialize DB client for %s: %v", conn.DBName, err)
            continue
        }
        app.dbClients[conn.DBName] = client
    }

    for _, conn := range config.Connections {
        cbConfig := config.GlobalConfig.CircuitBreakerConfig
        app.circuitBreakers[conn.DBName] = hystrix.NewClient(
            hystrix.WithTimeout(time.Duration(cbConfig.Timeout)*time.Millisecond),
            hystrix.WithMaxConcurrentRequests(cbConfig.MaxConcurrent),
            hystrix.WithErrorPercentThreshold(cbConfig.ErrorPercent),
            hystrix.WithSleepWindow(time.Duration(cbConfig.SleepWindow)*time.Millisecond),
        )
    }

    for i := 0; i < config.GlobalConfig.WorkerPoolSize; i++ {
        app.wg.Add(1)
        go app.worker()
    }

    app.scheduleQueries()
    app.server = app.startHTTPServer()

    go app.watchConfig(configFile)
    go app.watchCertificates()

    return app, nil
}

func (app *Application) worker() {
    defer app.wg.Done()
    for {
        select {
        case job := <-app.workerPool:
            app.executeQuery(job)
        case <-app.shutdown:
            return
        }
    }
}

func (app *Application) executeQuery(job QueryJob) {
    correlationID := fmt.Sprintf("%d", time.Now().UnixNano())
    logger := logrus.WithField("correlation_id", correlationID)

    client := app.dbClients[job.Connection.DBName]
    cb := app.circuitBreakers[job.Connection.DBName]

    start := time.Now()
    maxRetries := 3
    var results []float64
    var err error

    ctx, cancel := context.WithTimeout(job.Context, time.Duration(job.Query.Timeout)*time.Second)
    defer cancel()

    for attempt := 1; attempt <= maxRetries; attempt++ {
        state := cb.HealthCheck()
        circuitBreakerState.WithLabelValues(job.Connection.DBName).Set(float64(state))

        if state == hystrix.Open {
            app.dlq.Add(job)
            logger.Warn("Circuit breaker open, query sent to dead letter queue")
            return
        }

        res, err := cb.Execute(func() (interface{}, error) {
            return client.ExecuteQuery(ctx, job.Query.Query)
        }, nil)
        if err == nil {
            results = res.([]float64)
            break
        }

        retryAttempts.WithLabelValues(job.Query.Name, job.Query.DBType).Inc()
        logger.WithField("attempt", attempt).Warnf("Query failed: %v", err)
        time.Sleep(time.Duration(app.config.GlobalConfig.RetryConnInterval) * time.Second * attempt)
    }

    if err != nil {
        app.dlq.Add(job)
        errorCounter.WithLabelValues(job.Query.Name, job.Query.DBType).Inc()
        logger.Error("Query failed after max retries, sent to dead letter queue")
        return
    }

    duration := time.Since(start).Seconds()
    queryLatencyHist.WithLabelValues(job.Query.Name, job.Query.DBType).Observe(duration)

    for _, gauge := range job.Query.Gauges {
        value := results[gauge.Col-1]
        labels := utils.MergeLabels(job.Connection.ExtraLabels, gauge.ExtraLabels)
        prometheus.NewGauge(prometheus.GaugeOpts{
            Name:        gauge.Name,
            Help:        gauge.Desc,
            ConstLabels: labels,
        }).Set(value)
    }
}

func (app *Application) scheduleQueries() {
    type prioritizedJob struct {
        job      QueryJob
        priority int
    }

    jobs := make(chan prioritizedJob, len(app.config.Queries)*len(app.config.Connections))

    for _, query := range app.config.Queries {
        for _, conn := range app.config.Connections {
            if utils.ShouldRunQuery(query, conn) {
                go func(q Query, c Connection) {
                    ticker := time.NewTicker(time.Duration(q.TimeInterval) * time.Second)
                    defer ticker.Stop()
                    for {
                        select {
                        case <-ticker.C:
                            jobs <- prioritizedJob{
                                job: QueryJob{
                                    Query:      q,
                                    Connection: c,
                                    Context:    context.Background(),
                                },
                                priority: q.Priority,
                            }
                        case <-app.shutdown:
                            return
                        }
                    }
                }(query, conn)
            }
        }
    }

    go func() {
        for job := range jobs {
            app.workerPool <- job.job
            workerQueueGauge.Set(float64(len(app.workerPool)))
        }
    }()
}

func (app *Application) startHTTPServer() *http.Server {
    mux := http.NewServeMux()

    rateLimiter := ratelimit.NewBucketWithRate(
        float64(app.config.GlobalConfig.RateLimitRequests),
        int64(app.config.GlobalConfig.RateLimitBurst),
    )

    metricsHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if rateLimiter.TakeAvailable(1) == 0 {
            http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
            return
        }
        utils.BasicAuthHandler(app.config.BasicAuth.Username, app.config.BasicAuth.Password, promhttp.Handler())(w, r)
    })

    mux.Handle("/metrics", metricsHandler)
    mux.HandleFunc("/health", app.healthHandler)

    server := &http.Server{
        Addr:    fmt.Sprintf(":%d", app.config.GlobalConfig.Port),
        Handler: mux,
    }

    if app.config.GlobalConfig.UseHTTPS {
        tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}

        cert, err := tls.LoadX509KeyPair(app.config.GlobalConfig.CertFile, app.config.GlobalConfig.KeyFile)
        if err != nil {
            logrus.Fatalf("Failed to load server certificate: %v", err)
        }
        tlsConfig.Certificates = []tls.Certificate{cert}

        if app.config.GlobalConfig.PrometheusMTLSEnabled {
            caCert, err := ioutil.ReadFile(app.config.GlobalConfig.PrometheusClientCACert)
            if err != nil {
                logrus.Fatalf("Failed to read Prometheus CA cert: %v", err)
            }
            caCertPool := x509.NewCertPool()
            caCertPool.AppendCertsFromPEM(caCert)
            tlsConfig.ClientCAs = caCertPool
            tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
        }

        server.TLSConfig = tlsConfig
        go func() {
            if err := server.ListenAndServeTLS(app.config.GlobalConfig.CertFile, app.config.GlobalConfig.KeyFile); err != nil && err != http.ErrServerClosed {
                logrus.Fatalf("HTTPS server failed: %v", err)
            }
        }()
    } else {
        go func() {
            if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                logrus.Fatalf("HTTP server failed: %v", err)
            }
        }()
    }
    return server
}

func (app *Application) healthHandler(w http.ResponseWriter, r *http.Request) {
    status := struct {
        Databases map[string]string `json:"databases"`
    }{Databases: make(map[string]string)}

    for name, client := range app.dbClients {
        if err := client.Ping(); err != nil {
            status.Databases[name] = "unhealthy"
        } else {
            status.Databases[name] = "healthy"
        }
    }
    json.NewEncoder(w).Encode(status)
}

func (app *Application) Shutdown() {
    close(app.shutdown)
    app.wg.Wait()
    ctx, cancel := context.WithTimeout(context.Background(), time.Duration(app.config.GlobalConfig.ShutdownTimeout)*time.Second)
    defer cancel()
    if err := app.server.Shutdown(ctx); err != nil {
        logrus.Errorf("Server shutdown failed: %v", err)
    }
}

func (app *Application) watchConfig(filename string) {
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        logrus.Fatal(err)
    }
    defer watcher.Close()

    if err := watcher.Add(filename); err != nil {
        logrus.Fatal(err)
    }

    for {
        select {
        case event, ok := <-watcher.Events:
            if !ok {
                return
            }
            if event.Op&fsnotify.Write == fsnotify.Write {
                config, err := loadConfig(filename)
                if err != nil {
                    logrus.Errorf("Failed to reload config: %v", err)
                    continue
                }
                app.mu.Lock()
                app.config = config
                app.reloadDBClients() // Reload DB clients on config change
                app.mu.Unlock()
                logrus.Info("Configuration reloaded successfully")
            }
        case err, ok := <-watcher.Errors:
            if !ok {
                return
            }
            logrus.Errorf("Config watcher error: %v", err)
        }
    }
}

func (app *Application) watchCertificates() {
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        logrus.Fatal(err)
    }
    defer watcher.Close()

    certFiles := make(map[string]struct{})
    for _, conn := range app.config.Connections {
        if conn.TLSEnabled {
            certFiles[conn.TLSCertFile] = struct{}{}
            certFiles[conn.TLSKeyFile] = struct{}{}
            certFiles[conn.TLSCACertFile] = struct{}{}
        }
    }
    certFiles[app.config.GlobalConfig.CertFile] = struct{}{}
    certFiles[app.config.GlobalConfig.KeyFile] = struct{}{}
    certFiles[app.config.GlobalConfig.PrometheusClientCACert] = struct{}{}

    for file := range certFiles {
        if err := watcher.Add(file); err != nil {
            logrus.Errorf("Failed to watch certificate %s: %v", file, err)
        }
    }

    for {
        select {
        case event, ok := <-watcher.Events:
            if !ok {
                return
            }
            if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
                logrus.Info("Certificate change detected, reloading clients")
                app.reloadDBClients()
            }
        case err, ok := <-watcher.Errors:
            if !ok {
                return
            }
            logrus.Errorf("Certificate watcher error: %v", err)
        }
    }
}

func (app *Application) reloadDBClients() {
    app.mu.Lock()
    defer app.mu.Unlock()

    for _, conn := range app.config.Connections {
        if client, err := db.NewDBClient(conn); err == nil {
            oldClient := app.dbClients[conn.DBName]
            app.dbClients[conn.DBName] = client
            if oldClient != nil {
                oldClient.Close()
            }
        } else {
            logrus.Errorf("Failed to reload DB client for %s: %v", conn.DBName, err)
        }
    }
}

func loadConfig(filename string) (Config, error) {
    var config Config
    data, err := ioutil.ReadFile(filename)
    if err != nil {
        return config, err
    }
    if err := yaml.Unmarshal(data, &config); err != nil {
        return config, err
    }

    if config.GlobalConfig.EncryptionKey != "" {
        key := []byte(config.GlobalConfig.EncryptionKey)
        for i := range config.Connections {
            if decrypted, err := utils.Decrypt(key, config.Connections[i].DBPasswd); err == nil {
                config.Connections[i].DBPasswd = decrypted
            }
        }
        if decrypted, err := utils.Decrypt(key, config.BasicAuth.Password); err == nil {
            config.BasicAuth.Password = decrypted
        }
    }

    if config.GlobalConfig.WorkerPoolSize == 0 {
        config.GlobalConfig.WorkerPoolSize = 10
    }
    if config.GlobalConfig.ShutdownTimeout == 0 {
        config.GlobalConfig.ShutdownTimeout = 30
    }
    if config.GlobalConfig.RateLimitRequests == 0 {
        config.GlobalConfig.RateLimitRequests = 100
    }
    if config.GlobalConfig.RateLimitBurst == 0 {
        config.GlobalConfig.RateLimitBurst = 50
    }
    return config, nil
}