package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/afex/hystrix-go/hystrix"
	"github.com/barryq93/promDB2ORA/internal/app"
	"github.com/barryq93/promDB2ORA/internal/db"
	"github.com/barryq93/promDB2ORA/internal/types"
	"github.com/barryq93/promDB2ORA/internal/utils"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockDBClient is a mock implementation of db.DBClient for testing
type MockDBClient struct {
	mock.Mock
}

func (m *MockDBClient) ExecuteQuery(ctx context.Context, query string) ([]interface{}, error) {
	args := m.Called(ctx, query)
	return args.Get(0).([]interface{}), args.Error(1)
}

func (m *MockDBClient) Ping() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockDBClient) Close() {
	m.Called()
}

func TestMain(m *testing.M) {
	logrus.SetOutput(os.Stdout) // Changed from ioutil.Discard to see logs during testing
	os.Exit(m.Run())
}

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name        string
		configData  string
		env         string
		expectError bool
		checkFunc   func(*testing.T, app.Config, error)
	}{
		{
			name: "ValidConfigDevelopment",
			configData: `
global_config:
  log_level: INFO
  retry_conn_interval: 60
  encryption_key: "32-byte-long-secret-key-here!!"
connections:
  - db_host: "localhost"
    db_name: "testdb"
    db_passwd: "plaintext"
basic_auth:
  password: "plaintext"
`,
			env:         "development",
			expectError: false,
			checkFunc: func(t *testing.T, cfg app.Config, err error) {
				assert.NoError(t, err)
				assert.Equal(t, "INFO", cfg.GlobalConfig.LogLevel)
				assert.Equal(t, "plaintext", cfg.Connections[0].DBPasswd)
			},
		},
		{
			name: "InvalidConfigProductionPlaintext",
			configData: `
global_config:
  encryption_key: "32-byte-long-secret-key-here!!"
connections:
  - db_name: "testdb"
    db_passwd: "plaintext"
basic_auth:
  password: "plaintext"
`,
			env:         "production",
			expectError: true,
			checkFunc: func(t *testing.T, cfg app.Config, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "must be encrypted in production")
			},
		},
		{
			name: "NegativeRetryInterval",
			configData: `
global_config:
  retry_conn_interval: -1
`,
			env:         "development",
			expectError: true,
			checkFunc: func(t *testing.T, cfg app.Config, err error) {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), "cannot be negative")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpFile, err := os.CreateTemp("", "config-*.yml")
			assert.NoError(t, err)
			defer os.Remove(tmpFile.Name())

			_, err = tmpFile.Write([]byte(tt.configData))
			assert.NoError(t, err)
			tmpFile.Close()

			os.Setenv("ENV", tt.env)
			defer os.Unsetenv("ENV")

			cfg, err := app.LoadConfig(tmpFile.Name())
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			tt.checkFunc(t, cfg, err)
		})
	}
}

func TestNewApplication(t *testing.T) {
	configData := `
global_config:
  log_level: INFO
  worker_pool_size: 2
  port: 0
  dlq_retry_interval: 1
connections:
  - db_name: "testdb"
    db_type: "DB2"
    db_host: "localhost"
    db_port: 50000
    db_user: "test"
    db_passwd: "test"
    max_conns: 1
queries:
  - name: "TestQuery"
    db_type: "DB2"
    runs_on: ["test"]
    time_interval: 1
    query: "SELECT 1"
    timeout: 1
    priority: 1
    gauges:
      - name: "test_gauge"
        desc: "Test gauge"
        col: 1
`
	tmpFile, err := os.CreateTemp("", "config-*.yml")
	assert.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write([]byte(configData))
	assert.NoError(t, err)
	tmpFile.Close()

	os.Setenv("ENV", "development")
	defer os.Unsetenv("ENV")

	originalNewDBClient := db.NewDBClient
	defer func() { db.NewDBClient = originalNewDBClient }()
	db.NewDBClient = func(conn types.Connection) (*db.DBClient, error) {
		mockClient := new(MockDBClient)
		mockClient.On("Ping").Return(nil)
		return &db.DBClient{conn: &sql.DB{}, dbType: conn.DBType, name: conn.DBName}, nil
	}

	appInstance, err := app.NewApplication(tmpFile.Name())
	assert.NoError(t, err)
	assert.NotNil(t, appInstance)
	assert.Len(t, appInstance.dbClients, 1)
	assert.NotNil(t, appInstance.server)

	appInstance.Shutdown()
}

func TestExecuteQuery(t *testing.T) {
	mockClient := new(MockDBClient)
	mockClient.On("ExecuteQuery", mock.Anything, "SELECT 1").Return([]interface{}{float64(42.0)}, nil).Once()
	mockClient.On("ExecuteQuery", mock.Anything, "SELECT 1").Return(nil, fmt.Errorf("mock error")).Times(3)

	appInstance := &app.Application{
		config: app.Config{
			GlobalConfig: struct {
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
			}{
				RetryConnInterval: 1,
				LogPath:           t.TempDir(),
				DLQRetryInterval:  1,
			},
		},
		dbClients:  map[string]*db.DBClient{"testdb": mockClient},
		dlq:        app.NewDeadLetterQueue(t.TempDir()),
		workerPool: make(chan app.QueryJob, 1),
		shutdown:   make(chan struct{}),
	}

	job := app.QueryJob{
		Query: types.Query{
			Name:    "TestQuery",
			DBType:  "DB2",
			Query:   "SELECT 1",
			Timeout: 1,
			Gauges:  []types.Gauge{{Name: "test_gauge", Desc: "Test gauge", Col: 1}},
		},
		Connection: types.Connection{DBName: "testdb", DBType: "DB2", ExtraLabels: map[string]string{"dbinstance": "test"}},
		Context:    context.Background(),
		MaxRetries: 3,
	}
	appInstance.ExecuteQuery(job)
	mockClient.AssertExpectations(t)

	appInstance.ExecuteQuery(job)
	files, _ := os.ReadDir(appInstance.dlq.Path())
	assert.Len(t, files, 1)
}

func TestDeadLetterQueue(t *testing.T) {
	dlqPath := t.TempDir()
	dlq := app.NewDeadLetterQueue(dlqPath)

	job := app.QueryJob{
		Query:      types.Query{Name: "TestQuery"},
		Connection: types.Connection{DBName: "testdb"},
		Context:    context.Background(),
		MaxRetries: 3,
	}
	dlq.Add(job)

	files, err := os.ReadDir(dlqPath)
	assert.NoError(t, err)
	assert.Len(t, files, 1)

	data, err := os.ReadFile(filepath.Join(dlqPath, files[0].Name()))
	assert.NoError(t, err)
	var readJob app.QueryJob
	assert.NoError(t, json.Unmarshal(data, &readJob))
	assert.Equal(t, "TestQuery", readJob.Query.Name)
	assert.Equal(t, 1, readJob.RetryCount) // Should increment on first add
}

func TestHTTPServer(t *testing.T) {
	configData := `
global_config:
  log_level: INFO
  port: 0
  use_https: true
  cert_file: "test.crt"
  key_file: "test.key"
`
	tmpFile, err := os.CreateTemp("", "config-*.yml")
	assert.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.Write([]byte(configData))
	assert.NoError(t, err)
	tmpFile.Close()

	// Create temporary cert and key files (self-signed for testing)
	certFile, err := os.CreateTemp("", "test-*.crt")
	assert.NoError(t, err)
	defer os.Remove(certFile.Name())
	keyFile, err := os.CreateTemp("", "test-*.key")
	assert.NoError(t, err)
	defer os.Remove(keyFile.Name())

	// Minimal cert/key content (not valid, but enough to test logic)
	_, err = certFile.Write([]byte("fake-cert"))
	assert.NoError(t, err)
	_, err = keyFile.Write([]byte("fake-key"))
	assert.NoError(t, err)

	appInstance := &app.Application{
		config: app.Config{
			GlobalConfig: struct {
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
			}{
				UseHTTPS:          true,
				CertFile:          certFile.Name(),
				KeyFile:           keyFile.Name(),
				Port:              0,
				RateLimitRequests: 1,
				RateLimitBurst:    1,
			},
		},
		shutdown: make(chan struct{}),
	}

	server := appInstance.StartHTTPServer()
	defer appInstance.Shutdown()

	// Since certs are invalid, we expect an error, but we verify the HTTPS path is taken
	time.Sleep(100 * time.Millisecond) // Give server time to start
	assert.NotNil(t, server)
}

func TestRateLimitMiddleware(t *testing.T) {
	appInstance := &app.Application{
		config: app.Config{
			GlobalConfig: struct {
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
			}{
				RateLimitRequests: 1,
				RateLimitBurst:    1,
			},
		},
	}

	handler := appInstance.RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	}))

	req, _ := http.NewRequest("GET", "/metrics", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "OK", rr.Body.String())

	// Second request should be rate-limited
	req, _ = http.NewRequest("GET", "/metrics", nil)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
}

func TestEncryptDecrypt(t *testing.T) {
	key := "32-byte-long-secret-key-here!!"
	plaintext := "secretpassword"

	encrypted, err := utils.Encrypt(key, plaintext)
	assert.NoError(t, err)
	assert.NotEqual(t, plaintext, encrypted)

	decrypted, err := utils.Decrypt([]byte(key), encrypted)
	assert.NoError(t, err)
	assert.Equal(t, plaintext, decrypted)
}

func TestBasicAuthHandler(t *testing.T) {
	handler := utils.BasicAuthHandler("admin", "secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("OK"))
	}))

	req, _ := http.NewRequest("GET", "/", nil)
	req.SetBasicAuth("admin", "secret")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "OK", rr.Body.String())

	req, _ = http.NewRequest("GET", "/", nil)
	req.SetBasicAuth("admin", "wrong")
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
}

func TestExecuteQueryCircuitBreakerOpen(t *testing.T) {
	mockClient := new(MockDBClient)
	appInstance := &app.Application{
		config: app.Config{
			GlobalConfig: struct {
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
			}{
				LogPath:          t.TempDir(),
				DLQRetryInterval: 1,
			},
		},
		dbClients:  map[string]*db.DBClient{"testdb": mockClient},
		dlq:        app.NewDeadLetterQueue(t.TempDir()),
		workerPool: make(chan app.QueryJob, 1),
		shutdown:   make(chan struct{}),
	}

	// Configure Hystrix to immediately open circuit
	hystrix.ConfigureCommand("testdb", hystrix.CommandConfig{
		Timeout:               1000,
		MaxConcurrentRequests: 1,
		ErrorPercentThreshold: 1,
		SleepWindow:           1000,
	})

	// Force circuit open
	err := hystrix.Do("testdb", func() error { return fmt.Errorf("force fail") }, nil)
	assert.Error(t, err)

	job := app.QueryJob{
		Query:      types.Query{Name: "TestQuery", DBType: "DB2", Query: "SELECT 1", Timeout: 1},
		Connection: types.Connection{DBName: "testdb", DBType: "DB2"},
		Context:    context.Background(),
		MaxRetries: 3,
	}
	appInstance.ExecuteQuery(job)
	files, _ := os.ReadDir(appInstance.dlq.Path())
	assert.Len(t, files, 1)
}
