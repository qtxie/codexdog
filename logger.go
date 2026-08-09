package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const maxLogBytes int64 = 2 * 1024 * 1024
const logRotations = 3

var bearerSecret = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer\s+)[^\s,;]+`)
var namedSecret = regexp.MustCompile(`(?i)((api[-_ ]?key|token)\s*[:=]\s*)[^\s,;]+`)

func sanitizeText(value string) string {
	value = bearerSecret.ReplaceAllString(value, "${1}[REDACTED]")
	value = namedSecret.ReplaceAllString(value, "${1}[REDACTED]")
	runes := []rune(value)
	if len(runes) > 1000 {
		value = string(runes[:1000])
	}
	return value
}

type fileLogger struct {
	path string
	mu   sync.Mutex
}

func newLogger(path string) *fileLogger { return &fileLogger{path: path} }

func (l *fileLogger) Initialize() error {
	info, err := os.Stat(l.path)
	if err != nil || info.Size() < maxLogBytes {
		return nil
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", l.path, logRotations))
	for index := logRotations - 1; index >= 1; index-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.path, index), fmt.Sprintf("%s.%d", l.path, index+1))
	}
	return os.Rename(l.path, l.path+".1")
}

func (l *fileLogger) Log(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	file, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	line := time.Now().UTC().Format(time.RFC3339Nano) + " " + sanitizeText(strings.TrimRight(message, "\r\n")) + "\n"
	_, _ = file.WriteString(line)
}
