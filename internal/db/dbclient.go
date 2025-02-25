package db

import (
    "context"
    "crypto/tls"
    "crypto/x509"
    "database/sql"
    "fmt"
    "io/ioutil"
    "time"

    "github.com/example/db-monitoring-app/internal/app"
    "github.com/godror/godror"
    _ "github.com/ibm/go_ibm_db"
)

type DBClient struct {
    conn   *sql.DB
    dbType string
}

func NewDBClient(conn app.Connection) (*DBClient, error) {
    var db *sql.DB
    var err error

    tlsConfig := &tls.Config{}
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
            dsn += ";SECURITY=SSL"
            tlsName := fmt.Sprintf("tls_%s", conn.DBName)
            godror.RegisterTLSConfig(tlsName, tlsConfig)
            db, err = sql.Open("go_ibm_db", dsn+";TLS="+tlsName)
        } else {
            db, err = sql.Open("go_ibm_db", dsn)
        }
        if err != nil {
            return nil, fmt.Errorf("failed to open DB2 connection: %v", err)
        }
    case "Oracle":
        dsn := fmt.Sprintf("%s/%s@%s:%d/%s",
            conn.DBUser, conn.DBPasswd, conn.DBHost, conn.DBPort, conn.DBName)
        if conn.TLSEnabled {
            tlsName := fmt.Sprintf("tls_%s", conn.DBName)
            godror.RegisterTLSConfig(tlsName, tlsConfig)
            db, err = sql.Open("godror", dsn+"?ssl=true&ssl_verify=true&tls_config="+tlsName)
        } else {
            db, err = sql.Open("godror", dsn)
        }
        if err != nil {
            return nil, fmt.Errorf("failed to open Oracle connection: %v", err)
        }
    default:
        return nil, fmt.Errorf("unsupported database type: %s", conn.DBType)
    }

    db.SetMaxOpenConns(conn.MaxConns)
    db.SetConnMaxIdleTime(time.Duration(conn.IdleTimeout) * time.Second)
    return &DBClient{conn: db, dbType: conn.DBType}, nil
}

func (c *DBClient) ExecuteQuery(ctx context.Context, query string) ([]float64, error) {
    rows, err := c.conn.QueryContext(ctx, query)
    if err != nil {
        return nil, err
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
            return nil, err
        }
        for _, v := range values {
            results = append(results, *v.(*float64))
        }
    }
    return results, nil
}

func (c *DBClient) Ping() error {
    return c.conn.Ping()
}

func (c *DBClient) Close() {
    c.conn.Close()
}