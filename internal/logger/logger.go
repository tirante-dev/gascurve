// Package logger is a thin wrapper over zap. Production builds emit JSON,
// development builds emit a human friendly console format.
package logger

import (
	"fmt"
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Logger wraps a zap.SugaredLogger with key/value style methods.
type Logger struct {
	sugar *zap.SugaredLogger
}

// New builds a logger. level is one of debug, info, warn, error (case
// insensitive). dev selects the console encoder instead of JSON.
func New(level string, dev bool) (*Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	var cfg zap.Config
	if dev {
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		cfg = zap.NewProductionConfig()
		cfg.EncoderConfig.TimeKey = "ts"
		cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	}
	cfg.Level = zap.NewAtomicLevelAt(lvl)
	cfg.DisableStacktrace = true
	z, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("build zap logger: %w", err)
	}
	return FromZap(z), nil
}

// FromZap wraps an existing zap logger.
func FromZap(z *zap.Logger) *Logger {
	return &Logger{sugar: z.Sugar()}
}

// Nop returns a logger that discards everything.
func Nop() *Logger {
	return FromZap(zap.NewNop())
}

// ParseLevel converts a textual level into a zap level.
func ParseLevel(level string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "info":
		return zapcore.InfoLevel, nil
	case "debug":
		return zapcore.DebugLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.InfoLevel, fmt.Errorf("unknown log level %q", level)
	}
}

// With returns a child logger with the given key/value pairs attached.
func (l *Logger) With(kv ...any) *Logger {
	return &Logger{sugar: l.sugar.With(kv...)}
}

// Debug logs at debug level with key/value pairs.
func (l *Logger) Debug(msg string, kv ...any) { l.sugar.Debugw(msg, kv...) }

// Info logs at info level with key/value pairs.
func (l *Logger) Info(msg string, kv ...any) { l.sugar.Infow(msg, kv...) }

// Warn logs at warn level with key/value pairs.
func (l *Logger) Warn(msg string, kv ...any) { l.sugar.Warnw(msg, kv...) }

// Error logs at error level with key/value pairs.
func (l *Logger) Error(msg string, kv ...any) { l.sugar.Errorw(msg, kv...) }

// Fatal logs at fatal level and exits the process.
func (l *Logger) Fatal(msg string, kv ...any) { l.sugar.Fatalw(msg, kv...) }

// Sync flushes buffered entries. Errors from syncing stderr are ignored,
// which is the documented zap behavior on most platforms.
func (l *Logger) Sync() {
	if err := l.sugar.Sync(); err != nil && !isStdErrSyncError(err) {
		fmt.Fprintf(os.Stderr, "logger sync: %v\n", err)
	}
}

func isStdErrSyncError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "inappropriate ioctl") || strings.Contains(msg, "invalid argument") || strings.Contains(msg, "bad file descriptor")
}
