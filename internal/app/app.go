package app

import (
    "context"
    "crypto/tls"
    "crypto/x509"
    "encoding/json"
    "fmt"
    "io/ioutil"
    "net/http"
    "os"
    "sync"
    "time"

    "github.com/barryq93/promDB2ORA/internal/db"
    "github.com/barryq93/promDB2ORA/internal/utils"
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
        Env                    string `yaml:"env"`
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
    gauges         map[string]*prometheus.GaugeVec // Pre-registered gauges
}

type QueryJob struct {
    Query      Query
    Connection Connection
    Context    context.Context
    RetryCount int // Added for DLQ retry limit
}

func NewApplication(configFile string) (*Application, error) {
    config, err := loadConfig(configFile)
    if err != nil {
        return nil, fmt.Errorf("loading config: %v", err)
    }

    utils.SetLogLevel(config.GlobalConfig.LogLevel)

    app := &Application{
        config:         config,
        dbClients:      make(map[string]*db.DBClient),
        workerPool:     make(chan QueryJob, config.GlobalConfig.WorkerPoolSize),
        circuitBreakers: make(map[string]*hystrix.Client),
        dlq:            NewDeadLetterQueue(config.GlobalConfig.LogPath),
        shutdown:       make(chan struct{}),
        gauges:         make(map[string]*prometheus.GaugeVec),
    }

    for _, conn := range config.Connections {
        client, err := db.NewDBClient(conn)
        if err != nil {
            return nil, fmt.Errorf("initializing DB client for %s: %v", conn.DBName, err)
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

    app.loadCircuitBreakerState()
    app.initGauges() // Initialize gauges

    for i := 0; i < config.GlobalConfig.WorkerPoolSize; i++ {
        app.wg.Add(1)
        go app.worker()
    }

    app.scheduleQueries()
    app.server = app.startHTTPServer()
    app.dlq.ProcessRetries(app)

    go app.watchConfig(configFile)
    go app.watchCertificates()

    return app, nil
}

func (app *Application) initGauges() {
    for _, q := range app.config.Queries {
        for _, g := range q.Gauges {
            key := q.Name + "_" + g.Name
            app.gauges[key] = prometheus.NewGaugeVec(
                prometheus.GaugeOpts{
                    Name: g.Name,
                    Help: g.Desc,
                },
                []string{"dbinstance", "dbenv", "time"}, // All possible label keys
            )
            if err := prometheus.Register(app.gauges[key]); err != nil {
                logrus.Errorf("Failed to register gauge %s: %v", g.Name, err)
            }
        }
    }
}

func (app *Application) worker() {
    defer app.wg.Done()
    defer func() {
        if r := recover(); r != nil {
            logrus.Errorf("Worker recovered from panic: %v", r)
        }
    }()
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

        retryAttempts.WithLabelValues(job.Query.Name, job.Query.DBType, job.Connection.ExtraLabels["dbinstance"]).Inc()
        logger.WithField("attempt", attempt).Warnf("Query failed: %v", err)
        time.Sleep(time.Duration(app.config.GlobalConfig.RetryConnInterval) * time.Second * time.Duration(attempt))
    }

    if err != nil {
        app.dlq.Add(job)
        errorCounter.WithLabelValues(job.Query.Name, job.Query.DBType, job.Connection.ExtraLabels["dbinstance"]).Inc()
        logger.Error("Query failed after max retries, sent to dead letter queue")
        return
    }

    duration := time.Since(start).Seconds()
    queryLatencyHist.WithLabelValues(job.Query.Name, job.Query.DBType, job.Connection.ExtraLabels["dbinstance"]).Observe(duration)

    for _, gauge := range job.Query.Gauges {
        value := results[gauge.Col-1]
        labels := utils.MergeLabels(job.Connection.ExtraLabels, gauge.ExtraLabels)
        key := job.Query.Name + "_" + gauge.Name
        if g, ok := app.gauges[key]; ok {
            g.With(labels).Set(value)
        } else {
            logger.Errorf("Gauge %s not found for query %s", gauge.Name, job.Query.Name)
        }
    }
}

func (app *Application) scheduleQueries() {
    type prioritizedJob struct {
        job      QueryJob
        priority int
        nextRun  time.Time
    }

    jobs := make([]prioritizedJob, 0, len(app.config.Queries)*len(app.config.Connections))
    for _, query := range app.config.Queries {
        for _, conn := range app.config.Connections {
            if utils.ShouldRunQuery(query, conn) {
                jobs = append(jobs, prioritizedJob{
                    job:      QueryJob{Query: query, Connection: conn, Context: context.Background()},
                    priority: query.Priority,
                    nextRun:  time.Now(),
                })
            }
        }
    }

    go func() {
        ticker := time.NewTicker(time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                now := time.Now()
                for i := range jobs {
                    if now.After(jobs[i].nextRun) {
                        app.workerPool <- jobs[i].job
                        workerQueueGauge.Set(float64(len(app.workerPool)))
                        jobs[i].nextRun = now.Add(time.Duration(jobs[i].job.Query.TimeInterval) * time.Second)
                    }
                }
            case <-app.shutdown:
                return
            }
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
        tlsConfig := &tls.Config{
            MinVersion: tls.VersionTLS13,
            CipherSuites: []uint16{
                tls.TLS_AES_128_GCM_SHA256,
                tls.TLS_AES_256_GCM_SHA384,
            },
        }

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
            if err := server.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
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
        Databases       map[string]string `json:"databases"`
        WorkerPool      int               `json:"worker_pool"`
        CircuitBreakers map[string]int    `json:"circuit_breakers"`
    }{
        Databases:       make(map[string]string),
        CircuitBreakers: make(map[string]int),
    }

    for name, client := range app.dbClients {
        status.Databases[name] = "healthy"
        if err := client.Ping(); err != nil {
            status.Databases[name] = fmt.Sprintf("unhealthy: %v", err)
        }
    }
    status.WorkerPool = len(app.workerPool)
    for db, cb := range app.circuitBreakers {
        status.CircuitBreakers[db] = int(cb.HealthCheck())
    }
    w.Header().Set("Content-Type", "application/json")
    if err := json.NewEncoder(w).Encode(status); err != nil {
        logrus.Errorf("Failed to encode health response: %v", err)
    }
}

func (app *Application) Shutdown() {
    close(app.shutdown)
    app.wg.Wait()
    app.saveCircuitBreakerState()
    ctx, cancel := context.WithTimeout(context.Background(), time.Duration(app.config.GlobalConfig.ShutdownTimeout)*time.Second)
    defer cancel()
    if err := app.server.Shutdown(ctx); err != nil {
        logrus.Errorf("Server shutdown failed: %v", err)
    }
}

func (app *Application) watchConfig(filename string) {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        logrus.Fatalf("Failed to create config watcher: %v", err)
    }
    defer watcher.Close()

    if err := watcher.Add(filename); err != nil {
        logrus.Fatalf("Failed to watch config file: %v", err)
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
                app.reloadDBClients()
                app.mu.Unlock()
                logrus.Info("Configuration reloaded successfully")
            }
        case err, ok := <-watcher.Errors:
            if !ok {
                return
            }
            logrus.Errorf("Config watcher error: %v", err)
        case <-app.shutdown:
            return
        case <-ctx.Done():
            return
        }
    }
}

func (app *Application) watchCertificates() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        logrus.Fatalf("Failed to create certificate watcher: %v", err)
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
        case <-app.shutdown:
            return
        case <-ctx.Done():
            return
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

func (app *Application) saveCircuitBreakerState() {
    app.mu.Lock()
    defer app.mu.Unlock()
    state := make(map[string]int)
    for db, cb := range app.circuitBreakers {
        state[db] = int(cb.HealthCheck())
    }
    data, err := json.Marshal(state)
    if err != nil {
        logrus.Errorf("Failed to marshal circuit breaker state: %v", err)
        return
    }
    if err := ioutil.WriteFile("cb_state.json", data, 0644); err != nil {
        logrus.Errorf("Failed to save circuit breaker state: %v", err)
    }
}

func (app *Application) loadCircuitBreakerState() {
    data, err := ioutil.ReadFile("cb_state.json")
    if err != nil {
        logrus.Infof("No circuit breaker state file found: %v", err)
        return
    }
    state := make(map[string]int)
    if err := json.Unmarshal(data, &state); err != nil {
        logrus.Errorf("Failed to unmarshal circuit breaker state: %v", err)
        return
    }
    for db, cb := range app.circuitBreakers {
        if s, ok := state[db]; ok {
            switch s {
            case 1: // Open
                cb.Execute(func() (interface{}, error) { return nil, fmt.Errorf("force open") }, nil)
            case 2: // Half-open
                cb.Execute(func() (interface{}, error) { return nil, fmt.Errorf("half-open hint") }, nil)
            }
            logrus.Infof("Restored circuit breaker state for %s: %d", db, s)
        }
    }
}

func loadConfig(filename string) (Config, error) {
    var config Config
    data, err := ioutil.ReadFile(filename)
    if err != nil {
        return config, fmt.Errorf("reading file: %v", err)
    }
    if err := yaml.Unmarshal(data, &config); err != nil {
        return config, fmt.Errorf("unmarshaling YAML: %v", err)
    }

    if config.GlobalConfig.RetryConnInterval < 0 {
        return config, fmt.Errorf("retry_conn_interval cannot be negative")
    }
    for _, q := range config.Queries {
        if q.TimeInterval <= 0 {
            return config, fmt.Errorf("query %s: time_interval must be positive", q.Name)
        }
        if q.Timeout <= 0 {
            return config, fmt.Errorf("query %s: timeout must be positive", q.Name)
        }
    }

    env := config.GlobalConfig.Env
    if env == "" {
        env = os.Getenv("ENV")
    }
    if env == "" {
        env = "production"
        logrus.Warn("Environment not specified in config or ENV; defaulting to production")
    }
    isDev := env == "development"

    if config.GlobalConfig.EncryptionKey != "" {
        key := []byte(config.GlobalConfig.EncryptionKey)
        for i := range config.Connections {
            if !isEncrypted(config.Connections[i].DBPasswd) && !isDev {
                return config, fmt.Errorf("db_passwd for %s must be encrypted in production", config.Connections[i].DBName)
            }
            if decrypted, err := utils.Decrypt(key, config.Connections[i].DBPasswd); err == nil {
                config.Connections[i].DBPasswd = decrypted
            } else if !isDev {
                return config, fmt.Errorf("failed to decrypt db_passwd for %s: %v", config.Connections[i].DBName, err)
            }
        }
        if !isEncrypted(config.BasicAuth.Password) && !isDev {
            return config, fmt.Errorf("basic_auth.password must be encrypted in production")
        }
        if decrypted, err := utils.Decrypt(key, config.BasicAuth.Password); err == nil {
            config.BasicAuth.Password = decrypted
        } else if !isDev {
            return config, fmt.Errorf("failed to decrypt basic_auth.password: %v", err)
        }
    } else if !isDev {
        return config, fmt.Errorf("encryption_key must be set in production")
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

func isEncrypted(s string) bool {
    _, err := base64.StdEncoding.DecodeString(s)
    return err == nil && len(s) > 32
}