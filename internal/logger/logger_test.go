package logger

import (
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestNew(t *testing.T) {
	for _, tc := range []struct {
		level string
		dev   bool
		ok    bool
	}{
		{"debug", true, true},
		{"info", false, true},
		{"WARN", false, true},
		{"warning", true, true},
		{"error", false, true},
		{"", false, true},
		{"bogus", false, false},
	} {
		l, err := New(tc.level, tc.dev)
		if tc.ok != (err == nil) {
			t.Fatalf("New(%q, %v) err = %v", tc.level, tc.dev, err)
		}
		if l != nil {
			l.Debug("d", "k", 1)
			l.Info("i")
			l.Warn("w")
			l.Error("e")
			l.With("a", "b").Info("child")
			l.Sync()
		}
	}
}

func TestParseLevel(t *testing.T) {
	if lvl, err := ParseLevel("debug"); err != nil || lvl != zapcore.DebugLevel {
		t.Fatalf("debug: %v %v", lvl, err)
	}
	if _, err := ParseLevel("nope"); err == nil {
		t.Fatal("expected error")
	}
}

func TestObserved(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	l := FromZap(zap.New(core))
	l.Debug("debug", "k", "v")
	l.Info("info")
	l.Warn("warn")
	l.Error("error")
	l.With("x", 1).Info("with")
	if logs.Len() != 5 {
		t.Fatalf("expected 5 entries, got %d", logs.Len())
	}
	if logs.All()[4].ContextMap()["x"] != int64(1) {
		t.Fatalf("with field missing: %v", logs.All()[4].ContextMap())
	}
	l.Sync()
}

func TestFatalHook(t *testing.T) {
	core, _ := observer.New(zapcore.DebugLevel)
	l := FromZap(zap.New(core, zap.WithFatalHook(zapcore.WriteThenPanic)))
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic from fatal hook")
		}
	}()
	l.Fatal("boom")
}

func TestNop(t *testing.T) {
	Nop().Info("ignored")
	Nop().Sync()
}

func TestIsStdErrSyncError(t *testing.T) {
	if !isStdErrSyncError(errors.New("sync /dev/stderr: inappropriate ioctl for device")) {
		t.Fatal("expected true")
	}
	if isStdErrSyncError(errors.New("disk full")) {
		t.Fatal("expected false")
	}
}
