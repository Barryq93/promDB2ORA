package utils

import (
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "encoding/base64"
    "io"
    "net/http"

    "github.com/example/db-monitoring-app/internal/app"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/sirupsen/logrus"
)

func SetLogLevel(level string) {
    switch level {
    case "DEBUG":
        logrus.SetLevel(logrus.DebugLevel)
    case "INFO":
        logrus.SetLevel(logrus.InfoLevel)
    case "WARN":
        logrus.SetLevel(logrus.WarnLevel)
    case "ERROR":
        logrus.SetLevel(logrus.ErrorLevel)
    default:
        logrus.SetLevel(logrus.InfoLevel)
    }
}

func BasicAuthHandler(username, password string, h http.Handler) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        user, pass, ok := r.BasicAuth()
        if !ok || user != username || pass != password {
            w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
            http.Error(w, "Unauthorized", http.StatusUnauthorized)
            return
        }
        h.ServeHTTP(w, r)
    }
}

func MergeLabels(connLabels, gaugeLabels map[string]string) prometheus.Labels {
    result := make(prometheus.Labels)
    for k, v := range connLabels {
        result[k] = v
    }
    for k, v := range gaugeLabels {
        result[k] = v
    }
    return result
}

func ShouldRunQuery(query app.Query, conn app.Connection) bool {
    for _, tag := range query.RunsOn {
        for _, connTag := range conn.Tags {
            if tag == connTag {
                return query.DBType == conn.DBType
            }
        }
    }
    return false
}

func Encrypt(key, text string) (string, error) {
    block, err := aes.NewCipher([]byte(key))
    if err != nil {
        return "", err
    }
    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return "", err
    }
    nonce := make([]byte, gcm.NonceSize())
    if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
        return "", err
    }
    ciphertext := gcm.Seal(nil, nonce, []byte(text), nil)
    return base64.StdEncoding.EncodeToString(append(nonce, ciphertext...)), nil
}

func Decrypt(key []byte, encrypted string) (string, error) {
    data, err := base64.StdEncoding.DecodeString(encrypted)
    if err != nil {
        return "", err
    }
    block, err := aes.NewCipher(key)
    if err != nil {
        return "", err
    }
    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return "", err
    }
    nonceSize := gcm.NonceSize()
    if len(data) < nonceSize {
        return "", fmt.Errorf("invalid ciphertext")
    }
    nonce, ciphertext := data[:nonceSize], data[nonceSize:]
    plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
    if err != nil {
        return "", err
    }
    return string(plaintext), nil
}