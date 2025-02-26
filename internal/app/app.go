package app

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/barryq93/promDB2ORA/internal/db"
	"github.com/barryq93/promDB2ORA/internal/types"
	"github.com/barryq93/promDB2ORA/internal/utils"
	"github.com/gojek/heimdall/v7/hystrix"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// ========================
// 🔹 Global Prometheus Metrics
// ========================
var (
	logger = logrus.New()

	circuitBreakerState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "circuit_breaker_state",
			Help: "Current state of circuit breakers (0=closed, 1=open)",
		},
		[]string{"db_name"},
	)
	retryAttempts = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "query_retry_attempts_total",
			Help: "Total number of retry attempts",
		},
		[]string{"query_name", "db_type", "db_instance"},
	)
	errorCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "query_errors_total",
			Help: "Total number of query errors",
		},
		[]string{"query_name", "db_type", "db_instance"},
	)
	queryLatencyHist = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "query_execution_duration_seconds",
			Help:    "Duration of query execution in seconds",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
		},
		[]string{"query_name", "db_type", "db_instance"},
	)
)

// ========================
// 🔹 Config Struct
// ========================
type Config struct {
	GlobalConfig struct {
		Env                  string `yaml:"env"`
		LogLevel             string `yaml:"log_level"`
		RetryConnInterval    int    `yaml:"retry_conn_interval"`
		DefaultTimeInterval  int    `yaml:"default_time_interval"`
		LogPath              string `yaml:"log_path"`
		Port                 int    `yaml:"port"`
		UseHTTPS             bool   `yaml:"use_https"`
		CertFile             string `yaml:"cert_file"`
		KeyFile              string `yaml:"key_file"`
		ShutdownTimeout      int    `yaml:"shutdown_timeout"`
		WorkerPoolSize       int    `yaml:"worker_pool_size"`
		RateLimitRequests    int    `yaml:"rate_limit_requests"`
		RateLimitBurst       int    `yaml:"rate_limit_burst"`
		CircuitBreakerConfig struct {
			Timeout       int `yaml:"timeout"`
			MaxConcurrent int `yaml:"max_concurrent"`
			ErrorPercent  int `yaml:"error_percent"`
			SleepWindow   int `yaml:"sleep_window"`
		} `yaml:"circuit_breaker_config"`
	} `yaml:"global_config"`
	Queries     []types.Query      `yaml:"queries"`
	Connections []types.Connection `yaml:"connections"`
	BasicAuth   struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"basic_auth"`
}

// ========================
// 🔹 Application Struct
// ========================
type Application struct {
	config          Config
	dbClients       map[string]*db.DBClient
	workerPool      chan QueryJob
	circuitBreakers map[string]*hystrix.Client
	shutdown        chan struct{}
	wg              sync.WaitGroup
	server          *http.Server
	dlq             *DeadLetterQueue
}

// QueryJob Struct
type QueryJob struct {
	Query      types.Query
	Connection types.Connection
	Context    context.Context
}

// ========================
// 🔹 New Application Initialization
// ========================
func NewApplication(configFile string) (*Application, error) {
	config, err := LoadConfig(configFile)
	if err != nil {
		return nil, fmt.Errorf("loading config: %v", err)
	}

	utils.SetLogLevel(config.GlobalConfig.LogLevel)

	app := &Application{
		config:          config,
		dbClients:       make(map[string]*db.DBClient),
		workerPool:      make(chan QueryJob, config.GlobalConfig.WorkerPoolSize),
		circuitBreakers: make(map[string]*hystrix.Client),
		shutdown:        make(chan struct{}),
		dlq:             NewDeadLetterQueue(config.GlobalConfig.LogPath),
	}

	for _, conn := range config.Connections {
		client, err := db.NewDBClient(conn)
		if err != nil {
			return nil, fmt.Errorf("initializing DB client for %s: %v", conn.DBName, err)
		}
		app.dbClients[conn.DBName] = client
	}

	// Initialize circuit breakers
	for _, conn := range config.Connections {
		cbConfig := config.GlobalConfig.CircuitBreakerConfig
		app.circuitBreakers[conn.DBName] = hystrix.NewClient(
			hystrix.WithHTTPTimeout(time.Duration(cbConfig.Timeout)*time.Millisecond),
			hystrix.WithMaxConcurrentRequests(cbConfig.MaxConcurrent),
			hystrix.WithErrorPercentThreshold(cbConfig.ErrorPercent),
			hystrix.WithRetryCount(3),
		)
	}

	// Start workers
	for i := 0; i < config.GlobalConfig.WorkerPoolSize; i++ {
		app.wg.Add(1)
		go app.worker()
	}

	// Schedule queries
	app.scheduleQueries()

	// Start HTTP server
	app.server = app.startHTTPServer()

	// Start DLQ retry processor
	app.dlq.ProcessRetries(app)

	return app, nil
}

// ========================
// 🔹 Query Execution Worker
// ========================
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

// ========================
// 🔹 Query Execution with Circuit Breaker
// ========================
func (app *Application) executeQuery(job QueryJob) {
	logger := logrus.WithFields(logrus.Fields{
		"query":   job.Query.Name,
		"db_type": job.Query.DBType,
		"db_name": job.Connection.DBName,
	})

	cb := app.circuitBreakers[job.Connection.DBName]
	start := time.Now()

	results, err := app.dbClients[job.Connection.DBName].ExecuteQuery(job.Context, job.Query.Query)
	if err != nil {
		retryAttempts.WithLabelValues(job.Query.Name, job.Query.DBType, job.Connection.DBName).Inc()
		errorCounter.WithLabelValues(job.Query.Name, job.Query.DBType, job.Connection.DBName).Inc()
		logger.Error("Query failed after retries, sending to dead letter queue")
		app.dlq.Add(job)
		return
	}

	duration := time.Since(start).Seconds()
	queryLatencyHist.WithLabelValues(job.Query.Name, job.Query.DBType, job.Connection.DBName).Observe(duration)
	logger.Info("Query executed successfully")

	// Process results and update Prometheus metrics
	for _, gauge := range job.Query.Gauges {
		if gauge.Col <= len(results) {
			metric := prometheus.NewGauge(prometheus.GaugeOpts{
				Name:        gauge.Name,
				Help:        gauge.Desc,
				ConstLabels: utils.MergeLabels(job.Connection.ExtraLabels, gauge.ExtraLabels),
			})
			metric.Set(results[gauge.Col-1])
			prometheus.MustRegister(metric)
		}
	}
}

// ========================
// 🔹 Schedule Queries
// ========================
func (app *Application) scheduleQueries() {
	for _, query := range app.config.Queries {
		for _, conn := range app.config.Connections {
			if utils.ShouldRunQuery(query, conn) {
				app.wg.Add(1)
				go func(q types.Query, c types.Connection) {
					defer app.wg.Done()
					ticker := time.NewTicker(time.Duration(q.TimeInterval) * time.Second)
					defer ticker.Stop()
					for {
						select {
						case <-ticker.C:
							app.workerPool <- QueryJob{
								Query:      q,
								Connection: c,
								Context:    context.Background(),
							}
						case <-app.shutdown:
							return
						}
					}
				}(query, conn)
			}
		}
	}
}

// ========================
// 🔹 HTTP Server for Prometheus Metrics
// ========================
func (app *Application) startHTTPServer() *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", app.config.GlobalConfig.Port),
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil {
			logrus.Fatalf("HTTP server error: %v", err)
		}
	}()
	return server
}

// ========================
// 🔹 Graceful Shutdown
// ========================
func (app *Application) Shutdown() {
	close(app.shutdown)
	app.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(app.config.GlobalConfig.ShutdownTimeout)*time.Second)
	defer cancel()
	if err := app.server.Shutdown(ctx); err != nil {
		logrus.Errorf("Server shutdown error: %v", err)
	}
}

// ========================
// 🔹 Load YAML Configuration
// ========================
func LoadConfig(filename string) (Config, error) {
	var config Config
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return config, fmt.Errorf("reading file: %v", err)
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("unmarshaling YAML: %v", err)
	}
	return config, nil
}
