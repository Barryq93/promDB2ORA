package types

// Connection represents a database connection configuration.
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

// Query represents a database query configuration.
type Query struct {
	Name         string   `yaml:"name"`
	DBType       string   `yaml:"db_type"`
	RunsOn       []string `yaml:"runs_on"`
	TimeInterval int      `yaml:"time_interval"`
	Query        string   `yaml:"query"`
	Gauges       []Gauge  `yaml:"gauges"`
	Timeout      int      `yaml:"timeout,omitempty"`
	Priority     int      `yaml:"priority,omitempty"`
}

// Gauge represents a Prometheus gauge metric configuration.
type Gauge struct {
	Name        string            `yaml:"name"`
	Desc        string            `yaml:"desc"`
	Col         int               `yaml:"col"`
	ExtraLabels map[string]string `yaml:"extra_labels,omitempty"`
}
