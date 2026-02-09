package fs

import (
	"context"
	"testing"
	"time"

	"organ_codex/internal/domain"
)

type testPolicy struct {
	allowed bool
}

func (p testPolicy) CanFileOperation(_ context.Context, _, _ string, _ domain.FileOperation, _ string) (bool, string, error) {
	if p.allowed {
		return true, "allowed", nil
	}
	return false, "denied", nil
}

type testLogger struct {
	entries []domain.FileChangeLog
}

func (l *testLogger) LogFileChange(_ context.Context, entry domain.FileChangeLog) error {
	entry.CreatedAt = time.Now().UTC()
	l.entries = append(l.entries, entry)
	return nil
}

func TestWriteFileDeniedByPolicy(t *testing.T) {
	logger := &testLogger{}
	gw, err := NewGateway(t.TempDir(), testPolicy{allowed: false}, logger)
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}

	err = gw.WriteFile(context.Background(), "task-1", "coder", "outputs/a.txt", []byte("test"))
	if err == nil {
		t.Fatalf("expected write to be denied")
	}
	if len(logger.entries) == 0 {
		t.Fatalf("expected denied write to be logged")
	}
	if logger.entries[0].Allowed {
		t.Fatalf("expected denied log entry")
	}
}
