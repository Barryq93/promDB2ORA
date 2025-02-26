package db

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"io/ioutil"
	"time"

	"github.com/barryq93/promDB2ORA/internal/types"
	_ "github.com/ibmdb/go_ibm_db"
	"github.com/sirupsen/logrus"
)

type DBClient struct {
	conn   *sql.DB
	dbType string
	name   string
}

func NewDBClient(conn types.Connection) (*DBClient, error) {
	var db *sql.DB
	var err error

	tlsConfig := &tls.Config{
		ServerName: conn.DBHost,
		MinVersion: tls.VersionTLS13,
	}
	if conn.TLSEnabled {
		cert, err := tls.LoadX509KeyPair(conn.TLSCertFile, conn.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load client certificate: %v", err)
		}
		caCert, err := ioutil.ReadFile(conn.TLSCACertFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %v", err)
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig.Certificates = []tls.Certificate{cert}
		tlsConfig.RootCAs = caCertPool
	}

	switch conn.DBType {
	case "DB2":
		dsn := fmt.Sprintf("HOSTNAME=%s;PORT=%d;DATABASE=%s;UID=%s;PWD=%s",
			conn.DBHost, conn.DBPort, conn.DBName, conn.DBUser, conn.DBPasswd)
		if conn.TLSEnabled {
			dsn += ";SECURITY=SSL" // go_ibm_db handles TLS via SECURITY=SSL
		}
		db, err = sql.Open("go_ibm_db", dsn)
		if err != nil {
			return nil, fmt.Errorf("failed to open DB2 connection: %v", err)
		}
	case "Oracle":
		dsn := fmt.Sprintf("%s/%s@%s:%d/%s",
			conn.DBUser, conn.DBPasswd, conn.DBHost, conn.DBPort, conn.DBName)
		if conn.TLSEnabled {
			// For godror v0.44.2, use connection string parameters for SSL
			// Note: Custom TLSConfig isn't directly supported; certificates must be system-trusted or configured externally
			dsn += "?ssl=true&ssl_verify=true"
			// Warning: godror relies on system trust store or wallet for custom certs; tlsConfig isn't passed directly
			logrus.Warnf("TLS enabled for Oracle, but custom certificates (%s, %s, %s) may require system trust store configuration.",
				conn.TLSCertFile, conn.TLSKeyFile, conn.TLSCACertFile)
		}
		db, err = sql.Open("godror", dsn)
		if err != nil {
			return nil, fmt.Errorf("failed to open Oracle connection: %v", err)
		}
	default:
		return nil, fmt.Errorf("unsupported database type: %s", conn.DBType)
	}

	db.SetMaxOpenConns(conn.MaxConns)
	db.SetConnMaxIdleTime(time.Duration(conn.IdleTimeout) * time.Second)

	client := &DBClient{conn: db, dbType: conn.DBType, name: conn.DBName}
	if err := client.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("initial ping failed: %v", err)
	}
	return client, nil
}

func (c *DBClient) ExecuteQuery(ctx context.Context, query string) ([]float64, error) {
	stats := c.conn.Stats()
	if stats.OpenConnections >= stats.MaxOpenConnections {
		logrus.Warnf("DB connection pool for %s (%s) at capacity: %d/%d", c.name, c.dbType, stats.OpenConnections, stats.MaxOpenConnections)
	}

	rows, err := c.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query execution failed: %v", err)
	}
	defer rows.Close()

	var results []float64
	for rows.Next() {
		var values []interface{}
		columns, _ := rows.Columns()
		for range columns {
			values = append(values, new(float64))
		}
		if err := rows.Scan(values...); err != nil {
			return nil, fmt.Errorf("scanning row failed: %v", err)
		}
		for _, v := range values {
			results = append(results, *v.(*float64))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration failed: %v", err)
	}
	return results, nil
}

func (c *DBClient) Ping() error {
	return c.conn.Ping()
}

func (c *DBClient) Close() {
	if err := c.conn.Close(); err != nil {
		logrus.Errorf("Failed to close DB client for %s: %v", c.name, err)
	}
}
