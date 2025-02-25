package app

import (
    "encoding/json"
    "fmt"
    "io/ioutil"
    "os"
    "path/filepath"
    "sync"
    "time"

    "github.com/sirupsen/logrus"
)

type DeadLetterQueue struct {
    path string
    mu   sync.Mutex
}

func NewDeadLetterQueue(path string) *DeadLetterQueue {
    return &DeadLetterQueue{path: filepath.Join(path, "dead_letter")}
}

func (dlq *DeadLetterQueue) Add(job QueryJob) {
    dlq.mu.Lock()
    defer dlq.mu.Unlock()

    data, _ := json.Marshal(job)
    filename := fmt.Sprintf("%s/%d_%s.json", dlq.path, time.Now().UnixNano(), job.Query.Name)

    if err := os.MkdirAll(dlq.path, 0755); err != nil {
        logrus.Errorf("Failed to create DLQ directory: %v", err)
        return
    }
    if err := ioutil.WriteFile(filename, data, 0644); err != nil {
        logrus.Errorf("Failed to write to DLQ: %v", err)
    }
}