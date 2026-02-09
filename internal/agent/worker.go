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
	switch msg.Type {
	case domain.MessageTypeRequest:
		var req domain.TaskRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			p.logger.Printf("planner parse request failed: %v", err)
			_ = p.store.AckMessage(ctx, msg.ID, p.id, "bad payload")
			return
		}
		workReq := domain.WorkRequestPayload{
			Goal:               req.Goal,
			Scope:              req.Scope,
			AcceptanceCriteria: req.AcceptanceCriteria,
			TargetPaths:        req.TargetPaths,
		}
		payload, _ := json.Marshal(workReq)
		err := p.messenger.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      p.id,
			ToAgent:        "coder",
			Type:           domain.MessageTypeRequest,
			Payload:        payload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "planner-request-" + msg.ID,
		})
		result := "forwarded to coder"
		if err != nil {
			result = "failed to forward to coder: " + err.Error()
			p.logger.Printf("planner enqueue to coder failed: %v", err)
		}
		_ = p.store.AckMessage(ctx, msg.ID, p.id, result)
	case domain.MessageTypeDone:
		var res domain.WorkResultPayload
		if err := json.Unmarshal(msg.Payload, &res); err != nil {
			p.logger.Printf("planner parse done failed: %v", err)
			_ = p.store.AckMessage(ctx, msg.ID, p.id, "bad payload")
			return
		}
		finalPayload, _ := json.Marshal(res)
		err := p.messenger.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      p.id,
			ToAgent:        "orchestrator",
			Type:           domain.MessageTypeDone,
			Payload:        finalPayload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "planner-done-" + msg.ID,
		})
		result := "task marked done"
		if err != nil {
			result = "failed to notify orchestrator: " + err.Error()
			p.logger.Printf("planner notify done failed: %v", err)
		}
		_ = p.store.AckMessage(ctx, msg.ID, p.id, result)
	case domain.MessageTypeBlocked, domain.MessageTypeEscalate:
		_ = p.store.AckMessage(ctx, msg.ID, p.id, "planner received blocked/escalate")
	default:
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
	switch msg.Type {
	case domain.MessageTypeRequest:
		var req domain.WorkRequestPayload
		if err := json.Unmarshal(msg.Payload, &req); err != nil {
			_ = c.store.AckMessage(ctx, msg.ID, c.id, "bad payload")
			c.logger.Printf("coder parse request failed: %v", err)
			return
		}

		runCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
		defer cancel()
		plan, err := c.generatePlanWithCodex(runCtx, req)
		if err != nil {
			c.logger.Printf("coder codex generation failed: %v", err)
			payload := mustJSON(map[string]string{
				"reason": "codex generation failed: " + err.Error(),
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
			payload := mustJSON(map[string]string{
				"reason": "codex returned empty file list",
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

		createdFiles := make([]string, 0, len(plan.Files))
		for _, file := range plan.Files {
			if err := validateRelativePath(file.Path); err != nil {
				c.logger.Printf("coder invalid generated path %q: %v", file.Path, err)
				continue
			}
			content := []byte(file.Content)
			if err := c.files.WriteFile(ctx, msg.TaskID, c.id, file.Path, content); err != nil {
				c.logger.Printf("coder write file denied/failed for %s: %v", file.Path, err)
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
			}
			createdFiles = append(createdFiles, file.Path)
		}

		if len(createdFiles) == 0 {
			payload := mustJSON(map[string]string{
				"reason": "no files were created (policy denied or invalid paths)",
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
			Summary:      plan.Summary,
			CreatedFiles: createdFiles,
		})
		if err := c.messenger.EnqueueMessage(ctx, domain.Message{
			TaskID:         msg.TaskID,
			FromAgent:      c.id,
			ToAgent:        "planner",
			Type:           domain.MessageTypeDone,
			Payload:        donePayload,
			CorrelationID:  msg.CorrelationID,
			IdempotencyKey: "coder-done-" + msg.ID,
		}); err != nil {
			c.logger.Printf("coder send done failed: %v", err)
		}
		_ = c.store.AckMessage(ctx, msg.ID, c.id, fmt.Sprintf("completed, created %d files", len(createdFiles)))
	default:
		_ = c.store.AckMessage(ctx, msg.ID, c.id, "coder ignored message type")
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
