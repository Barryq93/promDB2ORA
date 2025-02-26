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
	dlq := &DeadLetterQueue{path: filepath.Join(path, "dead_letter")}
	if err := os.MkdirAll(dlq.path, 0755); err != nil {
		logrus.Errorf("Failed to create DLQ directory: %v", err)
	}
	return dlq
}

func (dlq *DeadLetterQueue) Add(job QueryJob) {
	dlq.mu.Lock()
	defer dlq.mu.Unlock()

	data, err := json.Marshal(job)
	if err != nil {
		logrus.Errorf("Failed to marshal DLQ job: %v", err)
		return
	}
	filename := fmt.Sprintf("%s/%d_%s.json", dlq.path, time.Now().UnixNano(), job.Query.Name)
	if err := ioutil.WriteFile(filename, data, 0644); err != nil {
		logrus.Errorf("Failed to write to DLQ: %v", err)
	}
}

func (dlq *DeadLetterQueue) ProcessRetries(app *Application) {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				dlq.mu.Lock()
				files, err := ioutil.ReadDir(dlq.path)
				if err != nil {
					logrus.Errorf("Failed to read DLQ directory: %v", err)
					dlq.mu.Unlock()
					continue
				}
				for _, file := range files {
					data, err := ioutil.ReadFile(filepath.Join(dlq.path, file.Name()))
					if err != nil {
						logrus.Errorf("Failed to read DLQ file %s: %v", file.Name(), err)
						continue
					}
					var job QueryJob
					if err := json.Unmarshal(data, &job); err != nil {
						logrus.Errorf("Failed to unmarshal DLQ job %s: %v", file.Name(), err)
						continue
					}
					app.workerPool <- job
					if err := os.Remove(filepath.Join(dlq.path, file.Name())); err != nil {
						logrus.Errorf("Failed to remove DLQ file %s: %v", file.Name(), err)
					}
				}
				dlq.mu.Unlock()
			case <-app.shutdown:
				return
			}
		}
	}()
}
