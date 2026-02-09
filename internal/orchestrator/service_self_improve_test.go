package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"organ_codex/internal/domain"
	"organ_codex/internal/messaging/inproc"
	"organ_codex/internal/policy"
	sqlitestore "organ_codex/internal/store/sqlite"
)

func TestDonePayloadInvalidBlocksTask(t *testing.T) {
	svc, store, shutdown := newHarness(t, Config{
		DispatchInterval: 5 * time.Millisecond,
		WatchdogInterval: 200 * time.Millisecond,
		IdleTimeout:      5 * time.Second,
		MaxRetries:       3,
	})
	defer shutdown()
	ctx := context.Background()

	task, err := svc.CreateTask(ctx, CreateTaskInput{Goal: "g", Scope: "s", OwnerAgent: "planner"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, domain.TaskStatusRunning, ""); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if err := svc.GrantChannel(ctx, domain.AgentChannel{
		TaskID:       task.ID,
		FromAgent:    "planner",
		ToAgent:      "orchestrator",
		AllowedTypes: []domain.MessageType{domain.MessageTypeDone},
		MaxMsgs:      10,
	}); err != nil {
		t.Fatalf("grant channel: %v", err)
	}

	if err := svc.EnqueueMessage(ctx, domain.Message{
		TaskID:         task.ID,
		FromAgent:      "planner",
		ToAgent:        "orchestrator",
		Type:           domain.MessageTypeDone,
		Payload:        json.RawMessage(`{"summary":""}`),
		CorrelationID:  "c1",
		IdempotencyKey: "c1",
	}); err != nil {
		t.Fatalf("enqueue done: %v", err)
	}

	got := waitTaskStatus(t, store, task.ID, 2*time.Second)
	if got != domain.TaskStatusBlocked {
		t.Fatalf("status=%s want=%s", got, domain.TaskStatusBlocked)
	}
}

func TestFinalStatusNotOverwrittenByLateMessage(t *testing.T) {
	svc, store, shutdown := newHarness(t, Config{
		DispatchInterval: 5 * time.Millisecond,
		WatchdogInterval: 200 * time.Millisecond,
		IdleTimeout:      5 * time.Second,
		MaxRetries:       3,
	})
	defer shutdown()
	ctx := context.Background()

	task, err := svc.CreateTask(ctx, CreateTaskInput{Goal: "g", Scope: "s", OwnerAgent: "planner"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, domain.TaskStatusRunning, ""); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if err := svc.GrantChannel(ctx, domain.AgentChannel{
		TaskID:       task.ID,
		FromAgent:    "planner",
		ToAgent:      "orchestrator",
		AllowedTypes: []domain.MessageType{domain.MessageTypeDone, domain.MessageTypeBlocked},
		MaxMsgs:      20,
	}); err != nil {
		t.Fatalf("grant channel: %v", err)
	}

	donePayload, _ := json.Marshal(domain.WorkResultPayload{
		Summary:      "ok",
		CreatedFiles: []string{"index.html"},
	})
	if err := svc.EnqueueMessage(ctx, domain.Message{
		TaskID:         task.ID,
		FromAgent:      "planner",
		ToAgent:        "orchestrator",
		Type:           domain.MessageTypeDone,
		Payload:        donePayload,
		CorrelationID:  "c-done",
		IdempotencyKey: "c-done",
	}); err != nil {
		t.Fatalf("enqueue done: %v", err)
	}
	if got := waitTaskStatus(t, store, task.ID, 2*time.Second); got != domain.TaskStatusDone {
		t.Fatalf("status=%s want=%s", got, domain.TaskStatusDone)
	}

	svc.handleOrchestratorMessage(ctx, domain.Message{
		ID:        "late-msg",
		TaskID:    task.ID,
		FromAgent: "planner",
		ToAgent:   "orchestrator",
		Type:      domain.MessageTypeBlocked,
		Payload:   json.RawMessage(`{"reason":"late"}`),
	})

	time.Sleep(120 * time.Millisecond)
	taskAfter, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if taskAfter.Status != domain.TaskStatusDone {
		t.Fatalf("late message changed final status to %s", taskAfter.Status)
	}
}

func TestWatchdogBlocksOnFailureAcks(t *testing.T) {
	svc, store, shutdown := newHarness(t, Config{
		DispatchInterval: 25 * time.Millisecond,
		WatchdogInterval: 5 * time.Second,
		IdleTimeout:      5 * time.Second,
		MaxRetries:       3,
	})
	defer shutdown()
	ctx := context.Background()

	task, err := svc.CreateTask(ctx, CreateTaskInput{Goal: "g", Scope: "s", OwnerAgent: "planner"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, domain.TaskStatusRunning, ""); err != nil {
		t.Fatalf("set running: %v", err)
	}

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		msgID := "m-fail-" + time.Now().Add(time.Duration(i)*time.Nanosecond).Format("150405.000000000")
		created, err := store.CreateMessage(ctx, domain.Message{
			ID:             msgID,
			TaskID:         task.ID,
			FromAgent:      "orchestrator",
			ToAgent:        "coder",
			Type:           domain.MessageTypeRequest,
			Payload:        json.RawMessage(`{"goal":"x"}`),
			CorrelationID:  "corr-fail",
			IdempotencyKey: "idem-fail-" + msgID,
			Status:         domain.MessageStatusDelivered,
			Attempts:       1,
			NextAttemptAt:  now,
			CreatedAt:      now,
		})
		if err != nil {
			t.Fatalf("create message: %v", err)
		}
		if !created {
			t.Fatalf("message not created")
		}
		if err := store.AckMessage(ctx, msgID, "coder", "failed to generate artifact"); err != nil {
			t.Fatalf("ack message: %v", err)
		}
	}

	svc.watchdogOnce(ctx)
	taskAfter, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if taskAfter.Status != domain.TaskStatusBlocked {
		t.Fatalf("status=%s want=%s", taskAfter.Status, domain.TaskStatusBlocked)
	}
}

func TestListTaskTimelineIncludesAllKinds(t *testing.T) {
	svc, store, shutdown := newHarness(t, Config{
		DispatchInterval: 25 * time.Millisecond,
		WatchdogInterval: 5 * time.Second,
		IdleTimeout:      5 * time.Second,
		MaxRetries:       3,
	})
	defer shutdown()
	ctx := context.Background()

	task, err := svc.CreateTask(ctx, CreateTaskInput{Goal: "g", Scope: "s", OwnerAgent: "planner"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	now := time.Now().UTC()
	created, err := store.CreateMessage(ctx, domain.Message{
		ID:             "m1",
		TaskID:         task.ID,
		FromAgent:      "orchestrator",
		ToAgent:        "coder",
		Type:           domain.MessageTypeRequest,
		Payload:        json.RawMessage(`{"goal":"x"}`),
		CorrelationID:  "corr1",
		IdempotencyKey: "idem1",
		Status:         domain.MessageStatusDelivered,
		Attempts:       1,
		NextAttemptAt:  now,
		CreatedAt:      now,
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}
	if !created {
		t.Fatalf("message not created")
	}
	if err := store.AckMessage(ctx, "m1", "coder", "ok"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if err := store.LogDecision(ctx, domain.DecisionLog{
		TaskID: task.ID,
		Actor:  "planner",
		Action: "forwarded_to_coder",
		Reason: "test",
	}); err != nil {
		t.Fatalf("decision: %v", err)
	}

	timeline, err := svc.ListTaskTimeline(ctx, task.ID, 100)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if len(timeline) < 3 {
		t.Fatalf("timeline too short: %d", len(timeline))
	}
	var hasMessage, hasAck, hasDecision bool
	for _, item := range timeline {
		switch item.Kind {
		case "message":
			hasMessage = true
		case "ack":
			hasAck = true
		case "decision":
			hasDecision = true
		}
	}
	if !hasMessage || !hasAck || !hasDecision {
		t.Fatalf("timeline kinds missing message=%t ack=%t decision=%t", hasMessage, hasAck, hasDecision)
	}
}

func TestStartTaskBlocksWhenRequestDeliveryRetriesExceeded(t *testing.T) {
	svc, store, shutdown := newHarness(t, Config{
		DispatchInterval: 5 * time.Millisecond,
		WatchdogInterval: 5 * time.Second,
		IdleTimeout:      5 * time.Second,
		MaxRetries:       1,
	})
	defer shutdown()
	ctx := context.Background()

	task, err := svc.CreateTask(ctx, CreateTaskInput{
		Goal:       "g",
		Scope:      "s",
		OwnerAgent: "unregistered-owner",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := svc.StartTask(ctx, task.ID); err != nil {
		t.Fatalf("start task: %v", err)
	}
	got := waitTaskStatus(t, store, task.ID, 2*time.Second)
	if got != domain.TaskStatusBlocked {
		t.Fatalf("status=%s want=%s", got, domain.TaskStatusBlocked)
	}
}

func TestDoneValidTransitionsToDone(t *testing.T) {
	svc, store, shutdown := newHarness(t, Config{
		DispatchInterval: 5 * time.Millisecond,
		WatchdogInterval: 200 * time.Millisecond,
		IdleTimeout:      5 * time.Second,
		MaxRetries:       3,
	})
	defer shutdown()
	ctx := context.Background()

	task, err := svc.CreateTask(ctx, CreateTaskInput{Goal: "g", Scope: "s", OwnerAgent: "planner"})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := store.UpdateTaskStatus(ctx, task.ID, domain.TaskStatusRunning, ""); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if err := svc.GrantChannel(ctx, domain.AgentChannel{
		TaskID:       task.ID,
		FromAgent:    "planner",
		ToAgent:      "orchestrator",
		AllowedTypes: []domain.MessageType{domain.MessageTypeDone},
		MaxMsgs:      10,
	}); err != nil {
		t.Fatalf("grant channel: %v", err)
	}
	donePayload, _ := json.Marshal(domain.WorkResultPayload{
		Summary:      "completed",
		CreatedFiles: []string{"index.html"},
	})
	if err := svc.EnqueueMessage(ctx, domain.Message{
		TaskID:         task.ID,
		FromAgent:      "planner",
		ToAgent:        "orchestrator",
		Type:           domain.MessageTypeDone,
		Payload:        donePayload,
		CorrelationID:  "done-ok",
		IdempotencyKey: "done-ok",
	}); err != nil {
		t.Fatalf("enqueue done: %v", err)
	}
	got := waitTaskStatus(t, store, task.ID, 2*time.Second)
	if got != domain.TaskStatusDone {
		t.Fatalf("status=%s want=%s", got, domain.TaskStatusDone)
	}
}

func newHarness(t *testing.T, cfg Config) (*Service, *sqlitestore.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	store, err := sqlitestore.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	bus := inproc.New(64)
	p := policy.New(store)
	svc := New(store, p, bus, cfg, log.New(io.Discard, "", 0))
	runCtx, cancel := context.WithCancel(context.Background())
	svc.Start(runCtx)
	return svc, store, func() {
		cancel()
		svc.Wait()
		_ = store.Close()
	}
}

func waitTaskStatus(t *testing.T, store *sqlitestore.Store, taskID string, timeout time.Duration) domain.TaskStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := store.GetTask(context.Background(), taskID)
		if err == nil && (task.Status == domain.TaskStatusDone || task.Status == domain.TaskStatusBlocked || task.Status == domain.TaskStatusFailed || task.Status == domain.TaskStatusCanceled) {
			return task.Status
		}
		time.Sleep(20 * time.Millisecond)
	}
	task, err := store.GetTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("get task after timeout: %v", err)
	}
	return task.Status
}
