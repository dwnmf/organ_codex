package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"organ_codex/internal/domain"
)

type MessageQueue interface {
	Register(agentID string) <-chan domain.Message
	Unregister(agentID string)
}

type Messenger interface {
	EnqueueMessage(ctx context.Context, msg domain.Message) error
}

type Store interface {
	AckMessage(ctx context.Context, messageID, agentID, result string) error
	CreateArtifact(ctx context.Context, artifact domain.Artifact) error
	LogDecision(ctx context.Context, entry domain.DecisionLog) error
}

type FileGateway interface {
	WriteFile(ctx context.Context, taskID, agentID, relPath string, content []byte) error
}

type codexPlan struct {
	Summary string      `json:"summary"`
	Files   []codexFile `json:"files"`
}

type codexFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type Planner struct {
	id        string
	queue     MessageQueue
	messenger Messenger
	store     Store
	logger    *log.Logger
}

func NewPlanner(queue MessageQueue, messenger Messenger, store Store, logger *log.Logger) *Planner {
	if logger == nil {
		logger = log.Default()
	}
	return &Planner{
		id:        "planner",
		queue:     queue,
		messenger: messenger,
		store:     store,
		logger:    logger,
	}
}

func (p *Planner) Start(ctx context.Context) {
	ch := p.queue.Register(p.id)
	go func() {
		defer p.queue.Unregister(p.id)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				p.handleMessage(ctx, msg)
			}
		}
	}()
}

func (p *Planner) handleMessage(ctx context.Context, msg domain.Message) {
	logAction(ctx, p.store, msg.TaskID, p.id, "message_received", "planner received message", map[string]any{
		"type":    msg.Type,
		"from":    msg.FromAgent,
		"to":      msg.ToAgent,
		"message": msg.ID,
	})

	switch msg.Type {
	case domain.MessageTypeRequest:
		var req domain.TaskRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			p.logger.Printf("planner parse request failed: %v", err)
			logAction(ctx, p.store, msg.TaskID, p.id, "message_parse_failed", "planner failed to parse request payload", map[string]any{
				"error": err.Error(),
			})
			_ = p.store.AckMessage(ctx, msg.ID, p.id, "bad payload")
			return
		}
		plan := domain.PlanProposalPayload{
			Summary: "two-step execution plan: implement then review",
			Nodes: []domain.PlanNode{
				{
					ID:      "implement",
					AgentID: "coder",
					Type:    domain.MessageTypeRequest,
					Goal:    req.Goal,
					Scope:   req.Scope,
				},
				{
					ID:        "review",
					AgentID:   "reviewer",
					Type:      domain.MessageTypeReview,
					DependsOn: []string{"implement"},
				},
			},
		}
		payload, _ := json.Marshal(plan)
		err := p.messenger.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      p.id,
			ToAgent:        "orchestrator",
			Type:           domain.MessageTypePropose,
			Payload:        payload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "planner-propose-" + msg.ID,
		})
		result := "plan proposed to orchestrator"
		if err != nil {
			result = "failed to propose plan: " + err.Error()
			p.logger.Printf("planner propose plan failed: %v", err)
			logAction(ctx, p.store, msg.TaskID, p.id, "plan_propose_failed", "planner failed to propose plan to orchestrator", map[string]any{
				"error": err.Error(),
			})
		} else {
			logAction(ctx, p.store, msg.TaskID, p.id, "plan_proposed", "planner proposed execution plan to orchestrator", map[string]any{
				"nodes":          len(plan.Nodes),
				"correlation_id": msg.CorrelationID,
			})
		}
		_ = p.store.AckMessage(ctx, msg.ID, p.id, result)
	case domain.MessageTypeBlocked, domain.MessageTypeEscalate:
		logAction(ctx, p.store, msg.TaskID, p.id, "blocked_or_escalated", "planner received blocked/escalate", map[string]any{
			"type": msg.Type,
		})
		_ = p.store.AckMessage(ctx, msg.ID, p.id, "planner received blocked/escalate")
	default:
		logAction(ctx, p.store, msg.TaskID, p.id, "message_ignored", "planner ignored unsupported message type", map[string]any{
			"type": msg.Type,
		})
		_ = p.store.AckMessage(ctx, msg.ID, p.id, "planner ignored message type")
	}
}

type Coder struct {
	id           string
	queue        MessageQueue
	messenger    Messenger
	store        Store
	files        FileGateway
	codexBinary  string
	codexWorkdir string
	logger       *log.Logger
}

type Reviewer struct {
	id        string
	queue     MessageQueue
	messenger Messenger
	store     Store
	logger    *log.Logger
}

func NewCoder(
	queue MessageQueue,
	messenger Messenger,
	store Store,
	files FileGateway,
	codexBinary string,
	codexWorkdir string,
	logger *log.Logger,
) *Coder {
	if logger == nil {
		logger = log.Default()
	}
	if strings.TrimSpace(codexBinary) == "" {
		codexBinary = "codex"
	}
	if strings.TrimSpace(codexWorkdir) == "" {
		codexWorkdir = "."
	}
	return &Coder{
		id:           "coder",
		queue:        queue,
		messenger:    messenger,
		store:        store,
		files:        files,
		codexBinary:  codexBinary,
		codexWorkdir: codexWorkdir,
		logger:       logger,
	}
}

func NewReviewer(queue MessageQueue, messenger Messenger, store Store, logger *log.Logger) *Reviewer {
	if logger == nil {
		logger = log.Default()
	}
	return &Reviewer{
		id:        "reviewer",
		queue:     queue,
		messenger: messenger,
		store:     store,
		logger:    logger,
	}
}

func (c *Coder) Start(ctx context.Context) {
	ch := c.queue.Register(c.id)
	go func() {
		defer c.queue.Unregister(c.id)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				c.handleMessage(ctx, msg)
			}
		}
	}()
}

func (c *Coder) handleMessage(ctx context.Context, msg domain.Message) {
	logAction(ctx, c.store, msg.TaskID, c.id, "message_received", "coder received message", map[string]any{
		"type":    msg.Type,
		"from":    msg.FromAgent,
		"to":      msg.ToAgent,
		"message": msg.ID,
	})

	switch msg.Type {
	case domain.MessageTypeRequest:
		var req domain.WorkRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			_ = c.store.AckMessage(ctx, msg.ID, c.id, "bad payload")
			c.logger.Printf("coder parse request failed: %v", err)
			logAction(ctx, c.store, msg.TaskID, c.id, "message_parse_failed", "coder failed to parse request payload", map[string]any{
				"error": err.Error(),
			})
			return
		}

		logAction(ctx, c.store, msg.TaskID, c.id, "codex_exec_started", "coder started codex generation", map[string]any{
			"goal":  trim(req.Goal, 180),
			"scope": trim(req.Scope, 180),
		})

		runCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		defer cancel()
		stopProgress := startProgressHeartbeat(runCtx, 5*time.Second, func(elapsed time.Duration) {
			logAction(context.Background(), c.store, msg.TaskID, c.id, "codex_exec_progress", "coder is generating with codex", map[string]any{
				"elapsed_sec": int(elapsed.Seconds()),
			})
		})
		plan, err := c.generatePlanWithCodex(runCtx, req)
		stopProgress()
		if err != nil {
			c.logger.Printf("coder codex generation failed: %v", err)
			logAction(ctx, c.store, msg.TaskID, c.id, "codex_exec_failed", "coder codex generation failed", map[string]any{
				"error": err.Error(),
			})
			payload := mustJSON(map[string]string{
				"reason":  "codex generation failed: " + err.Error(),
				"node_id": req.NodeID,
			})
			_ = c.messenger.EnqueueMessage(ctx, domain.Message{
				TaskID:         msg.TaskID,
				FromAgent:      c.id,
				ToAgent:        "orchestrator",
				Type:           domain.MessageTypeBlocked,
				Payload:        payload,
				CorrelationID:  msg.CorrelationID,
				IdempotencyKey: "coder-blocked-" + msg.ID,
			})
			_ = c.store.AckMessage(ctx, msg.ID, c.id, "generation failed")
			return
		}

		if len(plan.Files) == 0 {
			logAction(ctx, c.store, msg.TaskID, c.id, "codex_empty_plan", "codex returned empty file list", nil)
			payload := mustJSON(map[string]string{
				"reason":  "codex returned empty file list",
				"node_id": req.NodeID,
			})
			_ = c.messenger.EnqueueMessage(ctx, domain.Message{
				TaskID:         msg.TaskID,
				FromAgent:      c.id,
				ToAgent:        "orchestrator",
				Type:           domain.MessageTypeBlocked,
				Payload:        payload,
				CorrelationID:  msg.CorrelationID,
				IdempotencyKey: "coder-blocked-empty-" + msg.ID,
			})
			_ = c.store.AckMessage(ctx, msg.ID, c.id, "empty plan")
			return
		}
		logAction(ctx, c.store, msg.TaskID, c.id, "codex_exec_finished", "coder received file plan from codex", map[string]any{
			"files_count": len(plan.Files),
			"summary":     trim(plan.Summary, 160),
		})

		createdFiles := make([]string, 0, len(plan.Files))
		for _, file := range plan.Files {
			if err := validateRelativePath(file.Path); err != nil {
				c.logger.Printf("coder invalid generated path %q: %v", file.Path, err)
				logAction(ctx, c.store, msg.TaskID, c.id, "file_path_invalid", "generated path rejected", map[string]any{
					"path":  file.Path,
					"error": err.Error(),
				})
				continue
			}
			content := []byte(file.Content)
			if err := c.files.WriteFile(ctx, msg.TaskID, c.id, file.Path, content); err != nil {
				c.logger.Printf("coder write file denied/failed for %s: %v", file.Path, err)
				logAction(ctx, c.store, msg.TaskID, c.id, "file_write_failed", "failed to write generated file", map[string]any{
					"path":  file.Path,
					"error": err.Error(),
				})
				continue
			}

			sum := sha256.Sum256(content)
			checksum := hex.EncodeToString(sum[:])
			artifact := domain.Artifact{
				ID:            uuid.NewString(),
				TaskID:        msg.TaskID,
				ProducerAgent: c.id,
				Kind:          "file",
				URI:           file.Path,
				Checksum:      checksum,
				Metadata:      mustJSON(map[string]string{"source_message_id": msg.ID}),
				CreatedAt:     time.Now().UTC(),
			}
			if err := c.store.CreateArtifact(ctx, artifact); err != nil {
				c.logger.Printf("coder create artifact failed for %s: %v", file.Path, err)
				logAction(ctx, c.store, msg.TaskID, c.id, "artifact_create_failed", "failed to store artifact record", map[string]any{
					"path":  file.Path,
					"error": err.Error(),
				})
			}
			createdFiles = append(createdFiles, file.Path)
			logAction(ctx, c.store, msg.TaskID, c.id, "file_written", "generated file written", map[string]any{
				"path": file.Path,
				"size": len(content),
			})
		}

		if len(createdFiles) == 0 {
			logAction(ctx, c.store, msg.TaskID, c.id, "no_files_created", "all generated files failed validation/write", nil)
			payload := mustJSON(map[string]string{
				"reason":  "no files were created (policy denied or invalid paths)",
				"node_id": req.NodeID,
			})
			_ = c.messenger.EnqueueMessage(ctx, domain.Message{
				TaskID:         msg.TaskID,
				FromAgent:      c.id,
				ToAgent:        "orchestrator",
				Type:           domain.MessageTypeBlocked,
				Payload:        payload,
				CorrelationID:  msg.CorrelationID,
				IdempotencyKey: "coder-blocked-write-" + msg.ID,
			})
			_ = c.store.AckMessage(ctx, msg.ID, c.id, "no files created")
			return
		}

		donePayload, _ := json.Marshal(domain.WorkResultPayload{
			NodeID:       req.NodeID,
			Summary:      plan.Summary,
			CreatedFiles: createdFiles,
		})
		if err := c.messenger.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      c.id,
			ToAgent:        "orchestrator",
			Type:           domain.MessageTypeDone,
			Payload:        donePayload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "coder-done-" + msg.ID,
		}); err != nil {
			c.logger.Printf("coder send done failed: %v", err)
			logAction(ctx, c.store, msg.TaskID, c.id, "done_send_failed", "failed to send DONE to orchestrator", map[string]any{
				"error": err.Error(),
			})
		} else {
			logAction(ctx, c.store, msg.TaskID, c.id, "done_sent", "coder sent DONE to orchestrator", map[string]any{
				"created_files_count": len(createdFiles),
				"summary":             trim(plan.Summary, 160),
				"node_id":             req.NodeID,
			})
		}
		_ = c.store.AckMessage(ctx, msg.ID, c.id, fmt.Sprintf("completed, created %d files", len(createdFiles)))
	default:
		logAction(ctx, c.store, msg.TaskID, c.id, "message_ignored", "coder ignored unsupported message type", map[string]any{
			"type": msg.Type,
		})
		_ = c.store.AckMessage(ctx, msg.ID, c.id, "coder ignored message type")
	}
}

func (r *Reviewer) Start(ctx context.Context) {
	ch := r.queue.Register(r.id)
	go func() {
		defer r.queue.Unregister(r.id)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				r.handleMessage(ctx, msg)
			}
		}
	}()
}

func (r *Reviewer) handleMessage(ctx context.Context, msg domain.Message) {
	logAction(ctx, r.store, msg.TaskID, r.id, "message_received", "reviewer received message", map[string]any{
		"type":    msg.Type,
		"from":    msg.FromAgent,
		"to":      msg.ToAgent,
		"message": msg.ID,
	})

	switch msg.Type {
	case domain.MessageTypeReview:
		var req domain.ReviewRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			_ = r.store.AckMessage(ctx, msg.ID, r.id, "bad payload")
			logAction(ctx, r.store, msg.TaskID, r.id, "message_parse_failed", "reviewer failed to parse review payload", map[string]any{
				"error": err.Error(),
			})
			return
		}
		if strings.TrimSpace(req.Summary) == "" || len(req.CreatedFiles) == 0 {
			payload := mustJSON(map[string]string{
				"reason":  "review failed: empty summary or file list",
				"node_id": req.NodeID,
			})
			_ = r.messenger.EnqueueMessage(ctx, domain.Message{
				TaskID:         msg.TaskID,
				FromAgent:      r.id,
				ToAgent:        "orchestrator",
				Type:           domain.MessageTypeBlocked,
				Payload:        payload,
				CorrelationID:  msg.CorrelationID,
				IdempotencyKey: "reviewer-blocked-" + msg.ID,
			})
			_ = r.store.AckMessage(ctx, msg.ID, r.id, "review failed")
			logAction(ctx, r.store, msg.TaskID, r.id, "review_failed", "reviewer rejected result payload", map[string]any{
				"node_id":     req.NodeID,
				"summary_len": len(strings.TrimSpace(req.Summary)),
				"files_count": len(req.CreatedFiles),
			})
			return
		}

		donePayload, _ := json.Marshal(domain.WorkResultPayload{
			NodeID:       req.NodeID,
			Summary:      "Review passed: generated artifacts validated",
			CreatedFiles: req.CreatedFiles,
		})
		if err := r.messenger.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      r.id,
			ToAgent:        "orchestrator",
			Type:           domain.MessageTypeDone,
			Payload:        donePayload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "reviewer-done-" + msg.ID,
		}); err != nil {
			r.logger.Printf("reviewer send done failed: %v", err)
			logAction(ctx, r.store, msg.TaskID, r.id, "done_send_failed", "reviewer failed to send DONE", map[string]any{
				"error": err.Error(),
			})
			_ = r.store.AckMessage(ctx, msg.ID, r.id, "done send failed")
			return
		}
		logAction(ctx, r.store, msg.TaskID, r.id, "done_sent", "reviewer approved and sent DONE", map[string]any{
			"node_id":     req.NodeID,
			"files_count": len(req.CreatedFiles),
		})
		_ = r.store.AckMessage(ctx, msg.ID, r.id, "review completed")
	default:
		_ = r.store.AckMessage(ctx, msg.ID, r.id, "reviewer ignored message type")
		logAction(ctx, r.store, msg.TaskID, r.id, "message_ignored", "reviewer ignored unsupported message type", map[string]any{
			"type": msg.Type,
		})
	}
}

func (c *Coder) generatePlanWithCodex(ctx context.Context, req domain.WorkRequestPayload) (codexPlan, error) {
	schemaFile, err := os.CreateTemp("", "organ_codex_schema_*.json")
	if err != nil {
		return codexPlan{}, fmt.Errorf("create schema temp file: %w", err)
	}
	defer func() {
		_ = os.Remove(schemaFile.Name())
	}()
	defer schemaFile.Close()

	schema := `{
  "type":"object",
  "additionalProperties":false,
  "required":["summary","files"],
  "properties":{
    "summary":{"type":"string","minLength":1},
    "files":{
      "type":"array",
      "minItems":1,
      "items":{
        "type":"object",
        "additionalProperties":false,
        "required":["path","content"],
        "properties":{
          "path":{"type":"string","minLength":1},
          "content":{"type":"string"}
        }
      }
    }
  }
}`
	if _, err := schemaFile.WriteString(schema); err != nil {
		return codexPlan{}, fmt.Errorf("write schema file: %w", err)
	}

	outFile, err := os.CreateTemp("", "organ_codex_output_*.json")
	if err != nil {
		return codexPlan{}, fmt.Errorf("create output temp file: %w", err)
	}
	defer func() {
		_ = os.Remove(outFile.Name())
	}()
	outFile.Close()

	prompt := buildCodexPrompt(req)
	args := []string{
		"exec",
		"--skip-git-repo-check",
		"--output-schema",
		schemaFile.Name(),
		"-o",
		outFile.Name(),
		prompt,
	}
	cmd := exec.CommandContext(ctx, c.codexBinary, args...)
	cmd.Dir = c.codexWorkdir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return codexPlan{}, fmt.Errorf("codex exec failed: %w; output: %s", err, string(output))
	}

	raw, err := os.ReadFile(outFile.Name())
	if err != nil {
		return codexPlan{}, fmt.Errorf("read codex output: %w", err)
	}
	parsed, err := parseCodexOutput(raw)
	if err != nil {
		return codexPlan{}, fmt.Errorf("parse codex output: %w", err)
	}
	return parsed, nil
}

func buildCodexPrompt(req domain.WorkRequestPayload) string {
	var b strings.Builder
	b.WriteString("Generate implementation files for the task.\n")
	b.WriteString("Return only valid JSON matching the provided output schema.\n")
	b.WriteString("Do not wrap output in markdown fences.\n")
	b.WriteString("Paths must be relative, must not start with '/' or contain '..'.\n")
	b.WriteString("The result must be runnable and not a textual placeholder.\n\n")
	b.WriteString("If this is a browser app, prefer plain script loading that works when opening index.html directly.\n")
	b.WriteString("Avoid requiring build tools unless explicitly requested.\n")
	b.WriteString("Include a minimal README.md with run instructions.\n\n")
	b.WriteString("Task goal:\n")
	b.WriteString(req.Goal)
	b.WriteString("\n\nTask scope:\n")
	b.WriteString(req.Scope)
	b.WriteString("\n")
	if len(req.AcceptanceCriteria) > 0 {
		b.WriteString("\nAcceptance criteria:\n")
		for _, c := range req.AcceptanceCriteria {
			b.WriteString("- ")
			b.WriteString(c)
			b.WriteString("\n")
		}
	}
	if len(req.TargetPaths) > 0 {
		b.WriteString("\nPreferred paths (optional hints):\n")
		for _, p := range req.TargetPaths {
			b.WriteString("- ")
			b.WriteString(p)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func parseCodexOutput(raw []byte) (codexPlan, error) {
	text := strings.TrimSpace(string(raw))
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)

	var plan codexPlan
	if err := json.Unmarshal([]byte(text), &plan); err != nil {
		return codexPlan{}, err
	}
	return plan, nil
}

func validateRelativePath(p string) error {
	value := strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	value = strings.TrimPrefix(value, "./")
	if value == "" {
		return fmt.Errorf("empty path")
	}
	if strings.HasPrefix(value, "/") {
		return fmt.Errorf("absolute path is not allowed")
	}
	clean := filepath.Clean(value)
	if clean == "." {
		return fmt.Errorf("path resolves to current directory")
	}
	if strings.HasPrefix(clean, "..") {
		return fmt.Errorf("path escapes root")
	}
	return nil
}

func mustJSON(v any) []byte {
	payload, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return payload
}

func logAction(ctx context.Context, store Store, taskID string, actor string, action string, reason string, payload any) {
	if store == nil || taskID == "" || actor == "" || action == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	raw := []byte("{}")
	if payload != nil {
		raw = mustJSON(payload)
	}
	_ = store.LogDecision(ctx, domain.DecisionLog{
		TaskID:  taskID,
		Actor:   actor,
		Action:  action,
		Reason:  reason,
		Payload: raw,
	})
}

func startProgressHeartbeat(ctx context.Context, interval time.Duration, onTick func(elapsed time.Duration)) func() {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	stop := make(chan struct{})
	started := time.Now()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				if onTick != nil {
					onTick(time.Since(started))
				}
			}
		}
	}()

	return func() {
		close(stop)
	}
}

func trim(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
