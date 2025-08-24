package logger

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"time"

	"arbiter/internal/metrics"
)

type Logger struct {
	service string
	logger  *log.Logger
}

type LogLevel string

const (
	DEBUG LogLevel = "DEBUG"
	INFO  LogLevel = "INFO"
	WARN  LogLevel = "WARN"
	ERROR LogLevel = "ERROR"
)

type LogEntry struct {
	Timestamp string                 `json:"timestamp"`
	Level     LogLevel               `json:"level"`
	Service   string                 `json:"service"`
	TraceID   string                 `json:"trace_id,omitempty"`
	SpanID    string                 `json:"span_id,omitempty"`
	Message   string                 `json:"message"`
	Fields    map[string]interface{} `json:"fields,omitempty"`
}

var defaultLogger *Logger

func Init(service string) {
	defaultLogger = &Logger{
		service: service,
		logger:  log.New(os.Stdout, "", 0), // No prefix, we'll format everything
	}
}

func New(service string) *Logger {
	return &Logger{
		service: service,
		logger:  log.New(os.Stdout, "", 0),
	}
}

func WithContext(ctx context.Context) *LogContext {
	return &LogContext{
		ctx:    ctx,
		logger: defaultLogger,
		fields: make(map[string]interface{}),
	}
}

func FromContext(ctx context.Context) *LogContext {
	return WithContext(ctx)
}

func Default() *LogContext {
	return WithContext(context.Background())
}

type LogContext struct {
	ctx    context.Context
	logger *Logger
	fields map[string]interface{}
}

func (lc *LogContext) WithField(key string, value interface{}) *LogContext {
	lc.fields[key] = value
	return lc
}

func (lc *LogContext) WithFields(fields map[string]interface{}) *LogContext {
	maps.Copy(lc.fields, fields)
	return lc
}

func (lc *LogContext) Debug(message string) {
	lc.log(DEBUG, message)
}

func (lc *LogContext) Info(message string) {
	lc.log(INFO, message)
}

func (lc *LogContext) Warn(message string) {
	lc.log(WARN, message)
}

func (lc *LogContext) Error(message string) {
	lc.log(ERROR, message)
}

func (lc *LogContext) Errorf(format string, args ...interface{}) {
	lc.log(ERROR, fmt.Sprintf(format, args...))
}

func (lc *LogContext) log(level LogLevel, message string) {
	entry := LogEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Level:     level,
		Service:   lc.logger.service,
		Message:   message,
		Fields:    lc.fields,
	}

	if lc.ctx != nil {
		traceID := metrics.ExtractTraceID(lc.ctx)
		if traceID != "" {
			entry.TraceID = traceID
		}
	}

	data, err := json.Marshal(entry)
	if err != nil {
		lc.logger.logger.Printf("[%s] %s: %s", level, lc.logger.service, message)
		return
	}

	lc.logger.logger.Println(string(data))
}
