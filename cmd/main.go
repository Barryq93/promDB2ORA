package main

import (
    "flag"
    "fmt"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/barryq93/promDB2ORA/internal/app"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/sirupsen/logrus"
)

var (
    // Version and BuildTime set via -ldflags during build
    Version   = "unknown"
    BuildTime = "unknown"

    logger           = logrus.New()
    queryLatencyHist = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "query_execution_duration_seconds",
            Help:    "Duration of query execution in seconds",
            Buckets: prometheus.ExponentialBuckets(0.1, 2, 10),
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
    workerQueueGauge = prometheus.NewGauge(
        prometheus.GaugeOpts{
            Name: "worker_queue_length",
            Help: "Number of queries in the worker queue",
        },
    )
    circuitBreakerState = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{
            Name: "circuit_breaker_state",
            Help: "Current state of circuit breakers (0=closed, 1=open, 2=half-open)",
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
)

func init() {
    logger.SetFormatter(&logrus.JSONFormatter{
        FieldMap: logrus.FieldMap{
            logrus.FieldKeyMsg:  "message",
            logrus.FieldKeyTime: "timestamp",
        },
    })
    logger.SetOutput(os.Stdout)
    if level := os.Getenv("LOG_LEVEL"); level != "" {
        app.SetLogLevel(level)
    }

    metrics := []prometheus.Collector{queryLatencyHist, errorCounter, workerQueueGauge, circuitBreakerState, retryAttempts}
    for _, metric := range metrics {
        if err := prometheus.Register(metric); err != nil {
            logger.Errorf("Failed to register metric: %v", err)
        }
    }
}

func main() {
    configFile := flag.String("config", "config.yml", "Path to configuration file")
    version := flag.Bool("version", false, "Print version information and exit")
    flag.Parse()

    if *version {
        fmt.Printf("db-monitoring-app version %s (built %s)\n", Version, BuildTime)
        os.Exit(0)
    }

    application, err := app.NewApplication(*configFile)
    if err != nil {
        log.Fatalf("Failed to initialize application: %v", err)
    }

    sigChan := make(chan os.Signal, 1)
    signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

    logger.Info("Application started successfully")
    <-sigChan
    logger.Info("Shutdown signal received")
    application.Shutdown()
    logger.Info("Application shutdown complete")
}