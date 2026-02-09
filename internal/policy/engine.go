package policy

import (
	"context"
	"time"

	"organ_codex/internal/domain"
)

type Store interface {
	CheckTaskPermission(ctx context.Context, taskID, agentID string, operation domain.FileOperation, targetPath string, now time.Time) (bool, string, error)
	CanSendMessage(ctx context.Context, taskID, fromAgent, toAgent string, msgType domain.MessageType, now time.Time) (bool, string, error)
}

type Engine struct {
	store Store
}

func New(store Store) *Engine {
	return &Engine{store: store}
}

func (e *Engine) CanFileOperation(
	ctx context.Context,
	taskID string,
	agentID string,
	operation domain.FileOperation,
	targetPath string,
) (bool, string, error) {
	return e.store.CheckTaskPermission(ctx, taskID, agentID, operation, targetPath, time.Now().UTC())
}

func (e *Engine) CanMessage(
	ctx context.Context,
	taskID string,
	fromAgent string,
	toAgent string,
	msgType domain.MessageType,
) (bool, string, error) {
	return e.store.CanSendMessage(ctx, taskID, fromAgent, toAgent, msgType, time.Now().UTC())
}
