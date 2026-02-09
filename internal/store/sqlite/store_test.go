package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"organ_codex/internal/domain"
)

func TestPermissionAndChannelPolicy(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	taskID := uuid.NewString()
	if err := store.CreateTask(ctx, domain.Task{
		ID:         taskID,
		Goal:       "test",
		Scope:      "scope",
		OwnerAgent: "planner",
		Status:     domain.TaskStatusPlanned,
		MaxHops:    5,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	if err := store.GrantTaskPermission(ctx, domain.TaskPermission{
		TaskID:      taskID,
		AgentID:     "coder",
		Effect:      domain.PermissionEffectAllow,
		Operation:   domain.FileOperationWrite,
		PathPattern: "outputs/**",
	}); err != nil {
		t.Fatalf("grant allow permission: %v", err)
	}
	if err := store.GrantTaskPermission(ctx, domain.TaskPermission{
		TaskID:      taskID,
		AgentID:     "coder",
		Effect:      domain.PermissionEffectDeny,
		Operation:   domain.FileOperationWrite,
		PathPattern: "outputs/secret/**",
	}); err != nil {
		t.Fatalf("grant deny permission: %v", err)
	}

	allowed, _, err := store.CheckTaskPermission(ctx, taskID, "coder", domain.FileOperationWrite, "outputs/a.txt", time.Now().UTC())
	if err != nil {
		t.Fatalf("check allow permission: %v", err)
	}
	if !allowed {
		t.Fatalf("expected allow for outputs/a.txt")
	}
	denied, _, err := store.CheckTaskPermission(ctx, taskID, "coder", domain.FileOperationWrite, "outputs/secret/a.txt", time.Now().UTC())
	if err != nil {
		t.Fatalf("check deny permission: %v", err)
	}
	if denied {
		t.Fatalf("expected deny for outputs/secret/a.txt")
	}

	if err := store.GrantAgentChannel(ctx, domain.AgentChannel{
		TaskID:       taskID,
		FromAgent:    "planner",
		ToAgent:      "coder",
		AllowedTypes: []domain.MessageType{domain.MessageTypeRequest},
		MaxMsgs:      1,
	}); err != nil {
		t.Fatalf("grant channel: %v", err)
	}

	ok, _, err := store.CanSendMessage(ctx, taskID, "planner", "coder", domain.MessageTypeRequest, time.Now().UTC())
	if err != nil {
		t.Fatalf("can send first message: %v", err)
	}
	if !ok {
		t.Fatalf("expected first channel send to be allowed")
	}

	created, err := store.CreateMessage(ctx, domain.Message{
		ID:             uuid.NewString(),
		TaskID:         taskID,
		FromAgent:      "planner",
		ToAgent:        "coder",
		Type:           domain.MessageTypeRequest,
		Payload:        []byte(`{"goal":"x"}`),
		CorrelationID:  "corr-1",
		IdempotencyKey: "idem-1",
		Status:         domain.MessageStatusPending,
		CreatedAt:      time.Now().UTC(),
		NextAttemptAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create message with channel: %v", err)
	}
	if !created {
		t.Fatalf("expected channel-bound message to be created")
	}

	ok, _, err = store.CanSendMessage(ctx, taskID, "planner", "coder", domain.MessageTypeRequest, time.Now().UTC())
	if err != nil {
		t.Fatalf("can send second message: %v", err)
	}
	if ok {
		t.Fatalf("expected second send to be denied by max_msgs")
	}
}

func TestMessageIdempotency(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	taskID := uuid.NewString()
	if err := store.CreateTask(ctx, domain.Task{
		ID:         taskID,
		Goal:       "test",
		Scope:      "scope",
		OwnerAgent: "planner",
		Status:     domain.TaskStatusPlanned,
		MaxHops:    5,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	msg := domain.Message{
		ID:             uuid.NewString(),
		TaskID:         taskID,
		FromAgent:      "orchestrator",
		ToAgent:        "planner",
		Type:           domain.MessageTypeRequest,
		Payload:        []byte(`{"goal":"x"}`),
		CorrelationID:  "corr",
		IdempotencyKey: "idem-key",
		Status:         domain.MessageStatusPending,
		CreatedAt:      time.Now().UTC(),
		NextAttemptAt:  time.Now().UTC(),
	}
	created, err := store.CreateMessage(ctx, msg)
	if err != nil {
		t.Fatalf("create first message: %v", err)
	}
	if !created {
		t.Fatalf("expected first message to be created")
	}

	msg.ID = uuid.NewString()
	created, err = store.CreateMessage(ctx, msg)
	if err != nil {
		t.Fatalf("create duplicate message: %v", err)
	}
	if created {
		t.Fatalf("expected duplicate message to be ignored by idempotency")
	}
}

func TestAckMessageValidatesReceiver(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	taskID := uuid.NewString()
	if err := store.CreateTask(ctx, domain.Task{
		ID:         taskID,
		Goal:       "test",
		Scope:      "scope",
		OwnerAgent: "planner",
		Status:     domain.TaskStatusPlanned,
		MaxHops:    5,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	msg := domain.Message{
		ID:             uuid.NewString(),
		TaskID:         taskID,
		FromAgent:      "orchestrator",
		ToAgent:        "coder",
		Type:           domain.MessageTypeRequest,
		Payload:        []byte(`{"goal":"x"}`),
		CorrelationID:  "corr",
		IdempotencyKey: "idem",
		Status:         domain.MessageStatusDelivered,
		CreatedAt:      time.Now().UTC(),
		NextAttemptAt:  time.Now().UTC(),
	}
	created, err := store.CreateMessage(ctx, msg)
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if !created {
		t.Fatalf("expected message to be created")
	}

	if err := store.AckMessage(ctx, msg.ID, "planner", "ok"); err == nil {
		t.Fatalf("expected ack by non-receiver to fail")
	}
	if err := store.AckMessage(ctx, msg.ID, "coder", "ok"); err != nil {
		t.Fatalf("ack by receiver failed: %v", err)
	}

	msgs, err := store.ListTaskMessages(ctx, taskID, 10)
	if err != nil {
		t.Fatalf("list task messages: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatalf("expected at least one message")
	}
	if msgs[0].Status != domain.MessageStatusAcked {
		t.Fatalf("status=%s want=%s", msgs[0].Status, domain.MessageStatusAcked)
	}
}

func TestClaimAndExpireInFlightMessages(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	defer store.Close()

	taskID := uuid.NewString()
	if err := store.CreateTask(ctx, domain.Task{
		ID:         taskID,
		Goal:       "test",
		Scope:      "scope",
		OwnerAgent: "planner",
		Status:     domain.TaskStatusPlanned,
		MaxHops:    5,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	msg := domain.Message{
		ID:             uuid.NewString(),
		TaskID:         taskID,
		FromAgent:      "orchestrator",
		ToAgent:        "coder",
		Type:           domain.MessageTypeRequest,
		Payload:        []byte(`{"goal":"x"}`),
		CorrelationID:  "corr-claim",
		IdempotencyKey: "idem-claim",
		Status:         domain.MessageStatusPending,
		CreatedAt:      time.Now().UTC().Add(-time.Minute),
		NextAttemptAt:  time.Now().UTC().Add(-time.Minute),
	}
	created, err := store.CreateMessage(ctx, msg)
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if !created {
		t.Fatalf("expected message to be created")
	}

	claimed, err := store.ClaimMessageForDispatch(ctx, msg.ID, time.Now().UTC(), time.Now().UTC().Add(20*time.Second))
	if err != nil {
		t.Fatalf("claim message: %v", err)
	}
	if !claimed {
		t.Fatalf("expected message to be claimed")
	}

	updated, err := store.MarkMessageDelivered(ctx, msg.ID, time.Now().UTC().Add(-time.Second))
	if err != nil {
		t.Fatalf("mark delivered: %v", err)
	}
	if !updated {
		t.Fatalf("expected status transition to delivered")
	}

	expired, err := store.ListExpiredInFlightMessages(ctx, 10, time.Now().UTC())
	if err != nil {
		t.Fatalf("list expired in-flight messages: %v", err)
	}
	if len(expired) == 0 {
		t.Fatalf("expected expired in-flight message")
	}
	if expired[0].ID != msg.ID {
		t.Fatalf("unexpected expired message id=%s want=%s", expired[0].ID, msg.ID)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		store.Close()
		t.Fatalf("migrate store: %v", err)
	}
	return store
}
