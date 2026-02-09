package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"organ_codex/internal/domain"
)

var ErrForbiddenFileOperation = errors.New("file operation is forbidden by policy")

type Policy interface {
	CanFileOperation(ctx context.Context, taskID, agentID string, operation domain.FileOperation, targetPath string) (bool, string, error)
}

type ChangeLogger interface {
	LogFileChange(ctx context.Context, entry domain.FileChangeLog) error
}

type Gateway struct {
	root   string
	policy Policy
	logger ChangeLogger
}

func NewGateway(root string, policy Policy, logger ChangeLogger) (*Gateway, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root path: %w", err)
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create root path: %w", err)
	}
	return &Gateway{
		root:   absRoot,
		policy: policy,
		logger: logger,
	}, nil
}

func (g *Gateway) WriteFile(ctx context.Context, taskID, agentID, relPath string, content []byte) error {
	op := domain.FileOperationCreate
	absPath, normalized, err := g.resolve(relPath)
	if err != nil {
		_ = g.logger.LogFileChange(ctx, domain.FileChangeLog{
			TaskID:    taskID,
			AgentID:   agentID,
			Operation: op,
			Path:      relPath,
			Allowed:   false,
			Reason:    err.Error(),
			CreatedAt: time.Now().UTC(),
		})
		return err
	}
	if _, statErr := os.Stat(absPath); statErr == nil {
		op = domain.FileOperationWrite
	}

	allowed, reason, err := g.policy.CanFileOperation(ctx, taskID, agentID, op, normalized)
	if err != nil {
		return fmt.Errorf("policy check write file: %w", err)
	}
	if !allowed {
		_ = g.logger.LogFileChange(ctx, domain.FileChangeLog{
			TaskID:    taskID,
			AgentID:   agentID,
			Operation: op,
			Path:      normalized,
			Allowed:   false,
			Reason:    reason,
			CreatedAt: time.Now().UTC(),
		})
		return fmt.Errorf("%w: %s", ErrForbiddenFileOperation, reason)
	}

	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return fmt.Errorf("create parent directories: %w", err)
	}
	if err := os.WriteFile(absPath, content, 0o644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	if err := g.logger.LogFileChange(ctx, domain.FileChangeLog{
		TaskID:    taskID,
		AgentID:   agentID,
		Operation: op,
		Path:      normalized,
		Allowed:   true,
		Reason:    "allowed",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("log file write: %w", err)
	}
	return nil
}

func (g *Gateway) ReadFile(ctx context.Context, taskID, agentID, relPath string) ([]byte, error) {
	absPath, normalized, err := g.resolve(relPath)
	if err != nil {
		return nil, err
	}

	allowed, reason, err := g.policy.CanFileOperation(ctx, taskID, agentID, domain.FileOperationRead, normalized)
	if err != nil {
		return nil, fmt.Errorf("policy check read file: %w", err)
	}
	if !allowed {
		_ = g.logger.LogFileChange(ctx, domain.FileChangeLog{
			TaskID:    taskID,
			AgentID:   agentID,
			Operation: domain.FileOperationRead,
			Path:      normalized,
			Allowed:   false,
			Reason:    reason,
			CreatedAt: time.Now().UTC(),
		})
		return nil, fmt.Errorf("%w: %s", ErrForbiddenFileOperation, reason)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return content, nil
}

func (g *Gateway) resolve(relPath string) (absolute string, normalized string, err error) {
	normalized = strings.ReplaceAll(strings.TrimSpace(relPath), "\\", "/")
	normalized = strings.TrimPrefix(normalized, "./")
	normalized = strings.TrimPrefix(normalized, "/")
	if normalized == "" || normalized == "." {
		return "", "", fmt.Errorf("invalid relative path %q", relPath)
	}

	abs := filepath.Join(g.root, filepath.FromSlash(normalized))
	absClean := filepath.Clean(abs)
	absRoot := filepath.Clean(g.root)

	rel, err := filepath.Rel(absRoot, absClean)
	if err != nil {
		return "", "", fmt.Errorf("resolve relative path: %w", err)
	}
	if strings.HasPrefix(rel, "..") || rel == "." && normalized != "." {
		return "", "", fmt.Errorf("path escapes workspace root: %q", relPath)
	}
	return absClean, strings.ReplaceAll(rel, "\\", "/"), nil
}
