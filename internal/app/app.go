package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/afex/hystrix-go/hystrix"
	"github.com/barryq93/promDB2ORA/internal/db"
	"github.com/barryq93/promDB2ORA/internal/types"
	"github.com/barryq93/promDB2ORA/internal/utils"
	"github.com/fsnotify/fsnotify"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"
)

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
		DLQRetryInterval     int    `yaml:"dlq_retry_interval"`
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

type Application struct {
	config     Config
	mu         sync.RWMutex
	dbClients  map[string]*db.DBClient
	workerPool chan QueryJob
	shutdown   chan struct{}
	wg         sync.WaitGroup
	server     *http.Server
	dlq        *DeadLetterQueue
}

type QueryJob struct {
	Query      types.Query
	Connection types.Connection
	Context    context.Context
	RetryCount int
	MaxRetries int
}

func NewApplication(configFile string) (*Application, error) {
	config, err := LoadConfig(configFile)
	if err != nil {
		return nil, fmt.Errorf("loading config: %v", err)
	}

	utils.SetLogLevel(config.GlobalConfig.LogLevel)

	app := &Application{
		config:     config,
		dbClients:  make(map[string]*db.DBClient),
		workerPool: make(chan QueryJob, config.GlobalConfig.WorkerPoolSize),
		shutdown:   make(chan struct{}),
		dlq:        NewDeadLetterQueue(config.GlobalConfig.LogPath),
	}

	// Initialize DB clients
	for _, conn := range config.Connections {
		client, err := db.NewDBClient(conn)
		if err != nil {
			return nil, fmt.Errorf("failed to create DB client for %s: %v", conn.DBName, err)
		}
		app.dbClients[conn.DBName] = client

		cbConfig := config.GlobalConfig.CircuitBreakerConfig
		hystrix.ConfigureCommand(conn.DBName, hystrix.CommandConfig{
			Timeout:               cbConfig.Timeout,
			MaxConcurrentRequests: cbConfig.MaxConcurrent,
			ErrorPercentThreshold: cbConfig.ErrorPercent,
			SleepWindow:           cbConfig.SleepWindow,
		})
	}

	// Periodically update circuit breaker states
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				for _, conn := range config.Connections {
					circuit, _, err := hystrix.GetCircuit(conn.DBName)
					if err != nil {
						logrus.Errorf("Failed to get circuit state for %s: %v", conn.DBName, err)
						continue
					}
					if circuit == nil { // Circuit not yet created
						circuitBreakerState.WithLabelValues(conn.DBName).Set(0) // Default to closed
						continue
					}
					state := 0.0 // Closed
					if circuit.IsOpen() {
						state = 1 // Open
					}
					circuitBreakerState.WithLabelValues(conn.DBName).Set(state)
				}
			case <-app.shutdown:
				return
			}
		}
	}()

	for i := 0; i < config.GlobalConfig.WorkerPoolSize; i++ {
		app.wg.Add(1)
		go app.worker()
	}

	app.scheduleQueries()
	go app.watchConfig(configFile)
	app.server = app.startHTTPServer()

	app.dlq.ProcessRetries(app)
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
	logger := logrus.WithFields(logrus.Fields{
		"query":   job.Query.Name,
		"db_type": job.Query.DBType,
		"db_name": job.Connection.DBName,
	})

	start := time.Now()

	err := hystrix.Do(job.Connection.DBName, func() error {
		_, err := app.dbClients[job.Connection.DBName].ExecuteQuery(job.Context, job.Query.Query)
		return err
	}, nil)

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
}

func (app *Application) scheduleQueries() {
	app.mu.RLock()
	defer app.mu.RUnlock()
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
								MaxRetries: 3,
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

func (app *Application) rateLimitMiddleware(next http.Handler, limiter *rate.Limiter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow() {
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (app *Application) startHTTPServer() *http.Server {
	limiter := rate.NewLimiter(rate.Limit(app.config.GlobalConfig.RateLimitRequests), app.config.GlobalConfig.RateLimitBurst)
	mux := http.NewServeMux()
	mux.Handle("/metrics", app.rateLimitMiddleware(promhttp.Handler(), limiter))

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", app.config.GlobalConfig.Port),
		Handler: mux,
	}

	go func() {
		if app.config.GlobalConfig.UseHTTPS {
			if err := server.ListenAndServeTLS(app.config.GlobalConfig.CertFile, app.config.GlobalConfig.KeyFile); err != nil && err != http.ErrServerClosed {
				logrus.Fatalf("HTTPS server error: %v", err)
			}
		} else {
			if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logrus.Fatalf("HTTP server error: %v", err)
			}
		}
	}()
	return server
}

func (app *Application) Shutdown() {
	close(app.shutdown)
	app.wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(app.config.GlobalConfig.ShutdownTimeout)*time.Second)
	defer cancel()
	if err := app.server.Shutdown(ctx); err != nil {
		logrus.Errorf("Server shutdown error: %v", err)
	}
	for _, client := range app.dbClients {
		client.Close()
	}
}

func LoadConfig(filename string) (Config, error) {
	var config Config
	data, err := os.ReadFile(filename)
	if err != nil {
		return config, fmt.Errorf("reading file: %v", err)
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("unmarshaling YAML: %v", err)
	}
	return config, nil
}

func (app *Application) watchConfig(configFile string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logrus.Fatalf("Failed to create watcher: %v", err)
	}
	defer watcher.Close()
	if err := watcher.Add(configFile); err != nil {
		logrus.Fatalf("Failed to watch config file: %v", err)
	}
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				newConfig, err := LoadConfig(configFile)
				if err != nil {
					logrus.Errorf("Failed to reload config: %v", err)
					continue
				}
				app.mu.Lock()
				app.config = newConfig
				app.mu.Unlock()
				logrus.Info("Configuration reloaded")
			}
		case <-app.shutdown:
			return
		}
	}
}
