package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/barryq93/promDB2ORA/internal/app"
	"github.com/barryq93/promDB2ORA/internal/db"
	"github.com/barryq93/promDB2ORA/internal/types"
	"github.com/barryq93/promDB2ORA/internal/utils"
	"github.com/gojek/heimdall/v7/hystrix"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// MockDBClient is a mock implementation of db.DBClient for testing
type MockDBClient struct {
	mock.Mock
}

func (m *MockDBClient) ExecuteQuery(ctx context.Context, query string) ([]float64, error) {
	args := m.Called(ctx, query)
	return args.Get(0).([]float64), args.Error(1)
}

func (m *MockDBClient) Ping() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockDBClient) Close() {
	m.Called()
}

func TestMain(m *testing.M) {
	logrus.SetOutput(ioutil.Discard)
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
			tmpFile, err := ioutil.TempFile("", "config-*.yml")
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
	tmpFile, err := ioutil.TempFile("", "config-*.yml")
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
	mockClient.On("ExecuteQuery", mock.Anything, "SELECT 1").Return([]float64{42.0}, nil).Once()
	mockClient.On("ExecuteQuery", mock.Anything, "SELECT 1").Return(nil, fmt.Errorf("mock error")).Times(3)

	appInstance := &app.Application{
		config: app.Config{
			GlobalConfig: struct {
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
			}{
				RetryConnInterval: 1,
				LogPath:           t.TempDir(),
			},
		},
		dbClients:       map[string]*db.DBClient{"testdb": mockClient},
		circuitBreakers: map[string]*hystrix.Client{"testdb": hystrix.NewClient()},
		dlq:             app.NewDeadLetterQueue(t.TempDir()),
		workerPool:      make(chan app.QueryJob, 1),
		shutdown:        make(chan struct{}),
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
	}
	appInstance.ExecuteQuery(job)
	mockClient.AssertExpectations(t)

	appInstance.ExecuteQuery(job)
	files, _ := ioutil.ReadDir(appInstance.dlq.Path())
	assert.Len(t, files, 1)
}

func TestDeadLetterQueue(t *testing.T) {
	dlqPath := t.TempDir()
	dlq := app.NewDeadLetterQueue(dlqPath)

	job := app.QueryJob{
		Query:      types.Query{Name: "TestQuery"},
		Connection: types.Connection{DBName: "testdb"},
		Context:    context.Background(),
	}
	dlq.Add(job)

	files, err := ioutil.ReadDir(dlqPath)
	assert.NoError(t, err)
	assert.Len(t, files, 1)

	data, err := ioutil.ReadFile(filepath.Join(dlqPath, files[0].Name()))
	assert.NoError(t, err)
	var readJob app.QueryJob
	assert.NoError(t, json.Unmarshal(data, &readJob))
	assert.Equal(t, "TestQuery", readJob.Query.Name)
}

func TestHealthHandler(t *testing.T) {
	mockClient := new(MockDBClient)
	mockClient.On("Ping").Return(nil)

	appInstance := &app.Application{
		dbClients:  map[string]*db.DBClient{"testdb": mockClient},
		workerPool: make(chan app.QueryJob, 2),
		circuitBreakers: map[string]*hystrix.Client{
			"testdb": hystrix.NewClient(hystrix.WithTimeout(1000 * time.Millisecond)),
		},
	}
	appInstance.workerPool <- app.QueryJob{}

	req, _ := http.NewRequest("GET", "/health", nil)
	rr := httptest.NewRecorder()
	appInstance.HealthHandler(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	var status struct {
		Databases       map[string]string `json:"databases"`
		WorkerPool      int               `json:"worker_pool"`
		CircuitBreakers map[string]int    `json:"circuit_breakers"`
	}
	assert.NoError(t, json.Unmarshal(rr.Body.Bytes(), &status))
	assert.Equal(t, "healthy", status.Databases["testdb"])
	assert.Equal(t, 1, status.WorkerPool)
	assert.Equal(t, 0, status.CircuitBreakers["testdb"])
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
			}{LogPath: t.TempDir()},
		},
		dbClients:       map[string]*db.DBClient{"testdb": mockClient},
		circuitBreakers: map[string]*hystrix.Client{"testdb": hystrix.NewClient(hystrix.WithErrorPercentThreshold(1))},
		dlq:             app.NewDeadLetterQueue(t.TempDir()),
		workerPool:      make(chan app.QueryJob, 1),
		shutdown:        make(chan struct{}),
	}

	appInstance.circuitBreakers["testdb"].Execute(func() (interface{}, error) { return nil, fmt.Errorf("fail") }, nil)

	job := app.QueryJob{
		Query:      types.Query{Name: "TestQuery", DBType: "DB2", Query: "SELECT 1", Timeout: 1},
		Connection: types.Connection{DBName: "testdb", DBType: "DB2"},
		Context:    context.Background(),
	}
	appInstance.ExecuteQuery(job)
	files, _ := ioutil.ReadDir(appInstance.dlq.Path())
	assert.Len(t, files, 1)
}
