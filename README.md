# Database Monitoring Application

A Go application for monitoring DB2 and Oracle databases with Prometheus metrics.

## Features
- Secure TLS connections to databases and Prometheus endpoint
- Configurable via YAML with hot-reloading
- Concurrent query execution with worker pools
- Prometheus metrics with mTLS and basic auth
- Circuit breakers, dead letter queue, and query prioritization
- Rate limiting and configuration encryption
- Graceful shutdown and certificate rotation detection

## Prerequisites
- Go 1.20+
- IBM DB2 client libraries (set `IBM_DB_HOME`)
- Oracle Instant Client (add to `PATH`)
- Certificates in `certs/` directory

## Installation
```bash
git clone https://github.com/example/db-monitoring-app.git
cd db-monitoring-app
go mod download

Building

```bash

SET CGO_ENABLED=1
go build -o db-monitoring-app.exe cmd/main.go
```

Usage

Normal Run

```bash
./db-monitoring-app -config config.yml
```

Hotloading Config Changes (Windows)

```bash
hotload.bat
```
Configuration

See config.yml for a sample. Encrypt sensitive fields using the provided utility:

```bash
go run scripts/encrypt_password.go
```

Prometheus Integration
```yaml
scrape_configs:
  - job_name: 'db-monitoring'
    scheme: https
    tls_config:
      cert_file: /path/to/prometheus-client.crt
      key_file: /path/to/prometheus-client.key
      ca_file: /path/to/server-ca.crt
    basic_auth:
      username: admin
      password: securepassword
    metrics_path: /metrics
    static_configs:
      - targets: ['localhost:8080']
```

Contributing

Pull requests welcome! Include tests and update documentation.

License
MIT
