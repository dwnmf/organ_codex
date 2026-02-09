package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"organ_codex/internal/domain"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS tasks (
	id TEXT PRIMARY KEY,
	goal TEXT NOT NULL,
	scope TEXT NOT NULL,
	owner_agent TEXT NOT NULL,
	status TEXT NOT NULL,
	priority INTEGER NOT NULL DEFAULT 0,
	deadline_at INTEGER NULL,
	budget_tokens INTEGER NOT NULL DEFAULT 0,
	max_hops INTEGER NOT NULL DEFAULT 0,
	hop_count INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS task_permissions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	agent_id TEXT NOT NULL,
	effect TEXT NOT NULL,
	operation TEXT NOT NULL,
	path_pattern TEXT NOT NULL,
	expires_at INTEGER NULL,
	created_at INTEGER NOT NULL,
	FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_task_permissions_lookup ON task_permissions(task_id, agent_id, operation);

CREATE TABLE IF NOT EXISTS agent_channels (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	from_agent TEXT NOT NULL,
	to_agent TEXT NOT NULL,
	allowed_types TEXT NOT NULL,
	max_msgs INTEGER NOT NULL DEFAULT 0,
	sent_msgs INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER NULL,
	created_at INTEGER NOT NULL,
	FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_agent_channels_lookup ON agent_channels(task_id, from_agent, to_agent);

CREATE TABLE IF NOT EXISTS agent_messages (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	from_agent TEXT NOT NULL,
	to_agent TEXT NOT NULL,
	type TEXT NOT NULL,
	payload TEXT NOT NULL,
	correlation_id TEXT NOT NULL,
	idempotency_key TEXT NOT NULL,
	status TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL,
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_agent_messages_dispatch ON agent_messages(status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_agent_messages_task ON agent_messages(task_id, created_at);

CREATE TABLE IF NOT EXISTS message_acks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	message_id TEXT NOT NULL,
	agent_id TEXT NOT NULL,
	result TEXT NOT NULL,
	ack_at INTEGER NOT NULL,
	UNIQUE(message_id, agent_id),
	FOREIGN KEY(message_id) REFERENCES agent_messages(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS artifacts (
	id TEXT PRIMARY KEY,
	task_id TEXT NOT NULL,
	producer_agent TEXT NOT NULL,
	kind TEXT NOT NULL,
	uri TEXT NOT NULL,
	checksum TEXT NOT NULL,
	metadata TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS decision_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	actor TEXT NOT NULL,
	action TEXT NOT NULL,
	reason TEXT NOT NULL,
	payload TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_decision_log_task ON decision_log(task_id, created_at);

CREATE TABLE IF NOT EXISTS file_change_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT NOT NULL,
	agent_id TEXT NOT NULL,
	operation TEXT NOT NULL,
	path TEXT NOT NULL,
	allowed INTEGER NOT NULL,
	reason TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_file_change_log_task ON file_change_log(task_id, created_at);

CREATE TABLE IF NOT EXISTS idempotency_keys (
	key TEXT PRIMARY KEY,
	message_id TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`

type Store struct {
	db *sql.DB
}

func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	pragmas := []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA foreign_keys=ON;",
		"PRAGMA busy_timeout=5000;",
	}
	for _, stmt := range pragmas {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("set sqlite pragma %q: %w", stmt, err)
		}
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}
	return nil
}

func (s *Store) CreateTask(ctx context.Context, task domain.Task) error {
	now := time.Now().UTC()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	if task.UpdatedAt.IsZero() {
		task.UpdatedAt = now
	}
	if task.Status == "" {
		task.Status = domain.TaskStatusPlanned
	}
	if task.MaxHops <= 0 {
		task.MaxHops = 8
	}

	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO tasks(
			id, goal, scope, owner_agent, status, priority, deadline_at,
			budget_tokens, max_hops, hop_count, last_error, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		task.ID, task.Goal, task.Scope, task.OwnerAgent, string(task.Status), task.Priority,
		nullableUnix(task.DeadlineAt), task.BudgetTokens, task.MaxHops, task.HopCount, task.LastError,
		task.CreatedAt.Unix(), task.UpdatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("create task: %w", err)
	}
	return nil
}

func (s *Store) GetTask(ctx context.Context, taskID string) (domain.Task, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT id, goal, scope, owner_agent, status, priority, deadline_at, budget_tokens,
			max_hops, hop_count, last_error, created_at, updated_at
		FROM tasks WHERE id = ?`,
		taskID,
	)
	var t domain.Task
	var status string
	var deadline sql.NullInt64
	var created, updated int64
	if err := row.Scan(
		&t.ID, &t.Goal, &t.Scope, &t.OwnerAgent, &status, &t.Priority, &deadline,
		&t.BudgetTokens, &t.MaxHops, &t.HopCount, &t.LastError, &created, &updated,
	); err != nil {
		return domain.Task{}, fmt.Errorf("get task: %w", err)
	}
	t.Status = domain.TaskStatus(status)
	t.DeadlineAt = int64ToTimePtr(deadline)
	t.CreatedAt = unixToTime(created)
	t.UpdatedAt = unixToTime(updated)
	return t, nil
}

func (s *Store) ListTasks(ctx context.Context) ([]domain.Task, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, goal, scope, owner_agent, status, priority, deadline_at, budget_tokens,
			max_hops, hop_count, last_error, created_at, updated_at
		FROM tasks ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	result := make([]domain.Task, 0)
	for rows.Next() {
		var t domain.Task
		var status string
		var deadline sql.NullInt64
		var created, updated int64
		if err := rows.Scan(
			&t.ID, &t.Goal, &t.Scope, &t.OwnerAgent, &status, &t.Priority, &deadline,
			&t.BudgetTokens, &t.MaxHops, &t.HopCount, &t.LastError, &created, &updated,
		); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		t.Status = domain.TaskStatus(status)
		t.DeadlineAt = int64ToTimePtr(deadline)
		t.CreatedAt = unixToTime(created)
		t.UpdatedAt = unixToTime(updated)
		result = append(result, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tasks: %w", err)
	}
	return result, nil
}

func (s *Store) ListUnfinishedTasks(ctx context.Context) ([]domain.Task, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, goal, scope, owner_agent, status, priority, deadline_at, budget_tokens,
			max_hops, hop_count, last_error, created_at, updated_at
		FROM tasks
		WHERE status IN (?, ?, ?)
		ORDER BY updated_at ASC`,
		string(domain.TaskStatusPlanned), string(domain.TaskStatusRunning), string(domain.TaskStatusBlocked),
	)
	if err != nil {
		return nil, fmt.Errorf("list unfinished tasks: %w", err)
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		var t domain.Task
		var status string
		var deadline sql.NullInt64
		var created, updated int64
		if err := rows.Scan(
			&t.ID, &t.Goal, &t.Scope, &t.OwnerAgent, &status, &t.Priority, &deadline,
			&t.BudgetTokens, &t.MaxHops, &t.HopCount, &t.LastError, &created, &updated,
		); err != nil {
			return nil, fmt.Errorf("scan unfinished task: %w", err)
		}
		t.Status = domain.TaskStatus(status)
		t.DeadlineAt = int64ToTimePtr(deadline)
		t.CreatedAt = unixToTime(created)
		t.UpdatedAt = unixToTime(updated)
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unfinished tasks: %w", err)
	}
	return tasks, nil
}

func (s *Store) UpdateTaskStatus(ctx context.Context, taskID string, status domain.TaskStatus, lastError string) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE tasks SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		string(status), lastError, time.Now().UTC().Unix(), taskID,
	)
	if err != nil {
		return fmt.Errorf("update task status: %w", err)
	}
	return nil
}

func (s *Store) TouchTask(ctx context.Context, taskID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, time.Now().UTC().Unix(), taskID)
	if err != nil {
		return fmt.Errorf("touch task: %w", err)
	}
	return nil
}

func (s *Store) IncrementTaskHop(ctx context.Context, taskID string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx increment hop: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET hop_count = hop_count + 1, updated_at = ? WHERE id = ?`, time.Now().UTC().Unix(), taskID); err != nil {
		return 0, fmt.Errorf("update hop count: %w", err)
	}

	var hopCount int
	if err := tx.QueryRowContext(ctx, `SELECT hop_count FROM tasks WHERE id = ?`, taskID).Scan(&hopCount); err != nil {
		return 0, fmt.Errorf("read hop count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit increment hop: %w", err)
	}
	return hopCount, nil
}

func (s *Store) GrantTaskPermission(ctx context.Context, p domain.TaskPermission) error {
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO task_permissions(task_id, agent_id, effect, operation, path_pattern, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		p.TaskID, p.AgentID, string(p.Effect), string(p.Operation), normalizeRelPath(p.PathPattern),
		nullableUnix(p.ExpiresAt), time.Now().UTC().Unix(),
	)
	if err != nil {
		return fmt.Errorf("grant task permission: %w", err)
	}
	return nil
}

func (s *Store) CheckTaskPermission(
	ctx context.Context,
	taskID string,
	agentID string,
	operation domain.FileOperation,
	targetPath string,
	now time.Time,
) (bool, string, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT effect, path_pattern, expires_at
		FROM task_permissions
		WHERE task_id = ? AND agent_id = ? AND operation = ?`,
		taskID, agentID, string(operation),
	)
	if err != nil {
		return false, "", fmt.Errorf("query permissions: %w", err)
	}
	defer rows.Close()

	target := normalizeRelPath(targetPath)
	var allowMatch bool
	for rows.Next() {
		var effect string
		var pattern string
		var expires sql.NullInt64
		if err := rows.Scan(&effect, &pattern, &expires); err != nil {
			return false, "", fmt.Errorf("scan permission: %w", err)
		}
		if expires.Valid && now.Unix() > expires.Int64 {
			continue
		}
		if !globMatch(pattern, target) {
			continue
		}
		if domain.PermissionEffect(effect) == domain.PermissionEffectDeny {
			return false, "denied by explicit deny rule", nil
		}
		if domain.PermissionEffect(effect) == domain.PermissionEffectAllow {
			allowMatch = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, "", fmt.Errorf("iterate permissions: %w", err)
	}
	if allowMatch {
		return true, "allowed", nil
	}
	return false, "default deny (no matching allow rule)", nil
}

func (s *Store) GrantAgentChannel(ctx context.Context, ch domain.AgentChannel) error {
	allowedTypes, err := json.Marshal(ch.AllowedTypes)
	if err != nil {
		return fmt.Errorf("marshal allowed types: %w", err)
	}
	_, err = s.db.ExecContext(
		ctx,
		`INSERT INTO agent_channels(task_id, from_agent, to_agent, allowed_types, max_msgs, sent_msgs, expires_at, created_at)
		VALUES(?, ?, ?, ?, ?, 0, ?, ?)`,
		ch.TaskID, ch.FromAgent, ch.ToAgent, string(allowedTypes), ch.MaxMsgs, nullableUnix(ch.ExpiresAt), time.Now().UTC().Unix(),
	)
	if err != nil {
		return fmt.Errorf("grant agent channel: %w", err)
	}
	return nil
}

func (s *Store) CanSendMessage(
	ctx context.Context,
	taskID string,
	fromAgent string,
	toAgent string,
	msgType domain.MessageType,
	now time.Time,
) (bool, string, error) {
	if fromAgent == "orchestrator" {
		return true, "orchestrator privileged sender", nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", fmt.Errorf("begin tx can send message: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	rows, err := tx.QueryContext(
		ctx,
		`SELECT id, allowed_types, max_msgs, sent_msgs, expires_at
		FROM agent_channels
		WHERE task_id = ? AND from_agent = ? AND to_agent = ?`,
		taskID, fromAgent, toAgent,
	)
	if err != nil {
		return false, "", fmt.Errorf("query channels: %w", err)
	}
	defer rows.Close()

	var selectedID int64
	var denyReason string
	for rows.Next() {
		var channelID int64
		var allowedTypesRaw string
		var maxMsgs int
		var sentMsgs int
		var expires sql.NullInt64
		if err := rows.Scan(&channelID, &allowedTypesRaw, &maxMsgs, &sentMsgs, &expires); err != nil {
			return false, "", fmt.Errorf("scan channel: %w", err)
		}
		if expires.Valid && now.Unix() > expires.Int64 {
			denyReason = "channel expired"
			continue
		}
		allowedTypes, err := parseAllowedTypes(allowedTypesRaw)
		if err != nil {
			return false, "", fmt.Errorf("parse channel types: %w", err)
		}
		if !messageTypeAllowed(msgType, allowedTypes) {
			denyReason = "message type is not allowed by channel"
			continue
		}
		if maxMsgs > 0 && sentMsgs >= maxMsgs {
			denyReason = "channel max messages reached"
			continue
		}
		selectedID = channelID
		break
	}
	if err := rows.Err(); err != nil {
		return false, "", fmt.Errorf("iterate channels: %w", err)
	}

	if selectedID == 0 {
		if denyReason == "" {
			denyReason = "no matching channel"
		}
		return false, denyReason, nil
	}

	res, err := tx.ExecContext(
		ctx,
		`UPDATE agent_channels
		SET sent_msgs = sent_msgs + 1
		WHERE id = ? AND (max_msgs = 0 OR sent_msgs < max_msgs)`,
		selectedID,
	)
	if err != nil {
		return false, "", fmt.Errorf("increment channel usage: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, "", fmt.Errorf("channel usage affected rows: %w", err)
	}
	if affected == 0 {
		return false, "channel max messages reached", nil
	}

	if err := tx.Commit(); err != nil {
		return false, "", fmt.Errorf("commit can send message: %w", err)
	}
	return true, "allowed", nil
}

func (s *Store) CreateMessage(ctx context.Context, msg domain.Message) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx create message: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	res, err := tx.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO idempotency_keys(key, message_id, created_at)
		VALUES(?, ?, ?)`,
		msg.IdempotencyKey, msg.ID, time.Now().UTC().Unix(),
	)
	if err != nil {
		return false, fmt.Errorf("insert idempotency key: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("idempotency rows affected: %w", err)
	}
	if affected == 0 {
		return false, nil
	}

	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO agent_messages(
			id, task_id, from_agent, to_agent, type, payload, correlation_id, idempotency_key,
			status, attempts, next_attempt_at, last_error, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.TaskID, msg.FromAgent, msg.ToAgent, string(msg.Type), string(msg.Payload),
		msg.CorrelationID, msg.IdempotencyKey, string(msg.Status), msg.Attempts,
		msg.NextAttemptAt.Unix(), msg.LastError, msg.CreatedAt.Unix(),
	)
	if err != nil {
		return false, fmt.Errorf("insert message: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET updated_at = ? WHERE id = ?`, time.Now().UTC().Unix(), msg.TaskID); err != nil {
		return false, fmt.Errorf("touch task after message create: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit create message: %w", err)
	}
	return true, nil
}

func (s *Store) ListDispatchableMessages(ctx context.Context, limit int, now time.Time) ([]domain.Message, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, task_id, from_agent, to_agent, type, payload, correlation_id, idempotency_key,
			status, attempts, next_attempt_at, last_error, created_at
		FROM agent_messages
		WHERE status = ? AND next_attempt_at <= ?
		ORDER BY next_attempt_at ASC, created_at ASC
		LIMIT ?`,
		string(domain.MessageStatusPending), now.Unix(), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list dispatchable messages: %w", err)
	}
	defer rows.Close()

	result := make([]domain.Message, 0, limit)
	for rows.Next() {
		var m domain.Message
		var typ string
		var status string
		var nextAttempt, created int64
		var payload string
		if err := rows.Scan(
			&m.ID, &m.TaskID, &m.FromAgent, &m.ToAgent, &typ, &payload, &m.CorrelationID, &m.IdempotencyKey,
			&status, &m.Attempts, &nextAttempt, &m.LastError, &created,
		); err != nil {
			return nil, fmt.Errorf("scan dispatchable message: %w", err)
		}
		m.Type = domain.MessageType(typ)
		m.Payload = []byte(payload)
		m.Status = domain.MessageStatus(status)
		m.NextAttemptAt = unixToTime(nextAttempt)
		m.CreatedAt = unixToTime(created)
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dispatchable messages: %w", err)
	}
	return result, nil
}

func (s *Store) MarkMessageDelivered(ctx context.Context, messageID string) error {
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE agent_messages
		SET status = ?, last_error = ''
		WHERE id = ?`,
		string(domain.MessageStatusDelivered), messageID,
	)
	if err != nil {
		return fmt.Errorf("mark message delivered: %w", err)
	}
	return nil
}

func (s *Store) MarkMessageForRetry(ctx context.Context, messageID string, lastError string, retryAt time.Time, maxRetries int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx message retry: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT attempts FROM agent_messages WHERE id = ?`, messageID).Scan(&attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("message not found: %s", messageID)
		}
		return false, fmt.Errorf("get message attempts: %w", err)
	}

	nextAttempts := attempts + 1
	if nextAttempts >= maxRetries {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE agent_messages SET status = ?, attempts = ?, last_error = ? WHERE id = ?`,
			string(domain.MessageStatusFailed), nextAttempts, lastError, messageID,
		); err != nil {
			return false, fmt.Errorf("mark message failed after retries: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("commit message fail: %w", err)
		}
		return false, nil
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE agent_messages
		SET status = ?, attempts = ?, next_attempt_at = ?, last_error = ?
		WHERE id = ?`,
		string(domain.MessageStatusPending), nextAttempts, retryAt.Unix(), lastError, messageID,
	); err != nil {
		return false, fmt.Errorf("schedule message retry: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit message retry: %w", err)
	}
	return true, nil
}

func (s *Store) AckMessage(ctx context.Context, messageID string, agentID string, result string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx ack message: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	_, err = tx.ExecContext(
		ctx,
		`INSERT OR IGNORE INTO message_acks(message_id, agent_id, result, ack_at)
		VALUES(?, ?, ?, ?)`,
		messageID, agentID, result, time.Now().UTC().Unix(),
	)
	if err != nil {
		return fmt.Errorf("ack message: %w", err)
	}

	_, err = tx.ExecContext(
		ctx,
		`UPDATE tasks
		SET updated_at = ?
		WHERE id = (SELECT task_id FROM agent_messages WHERE id = ?)`,
		time.Now().UTC().Unix(),
		messageID,
	)
	if err != nil {
		return fmt.Errorf("touch task on ack: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ack message: %w", err)
	}
	return nil
}

func (s *Store) CreateArtifact(ctx context.Context, artifact domain.Artifact) error {
	if artifact.CreatedAt.IsZero() {
		artifact.CreatedAt = time.Now().UTC()
	}
	if artifact.Metadata == nil {
		artifact.Metadata = []byte("{}")
	}
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO artifacts(id, task_id, producer_agent, kind, uri, checksum, metadata, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.ID, artifact.TaskID, artifact.ProducerAgent, artifact.Kind, artifact.URI,
		artifact.Checksum, string(artifact.Metadata), artifact.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("create artifact: %w", err)
	}
	return nil
}

func (s *Store) LogDecision(ctx context.Context, entry domain.DecisionLog) error {
	payload := string(entry.Payload)
	if payload == "" {
		payload = "{}"
	}
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO decision_log(task_id, actor, action, reason, payload, created_at)
		VALUES(?, ?, ?, ?, ?, ?)`,
		entry.TaskID, entry.Actor, entry.Action, entry.Reason, payload, time.Now().UTC().Unix(),
	)
	if err != nil {
		return fmt.Errorf("log decision: %w", err)
	}
	return nil
}

func (s *Store) LogFileChange(ctx context.Context, entry domain.FileChangeLog) error {
	allowed := 0
	if entry.Allowed {
		allowed = 1
	}
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO file_change_log(task_id, agent_id, operation, path, allowed, reason, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		entry.TaskID, entry.AgentID, string(entry.Operation), normalizeRelPath(entry.Path),
		allowed, entry.Reason, time.Now().UTC().Unix(),
	)
	if err != nil {
		return fmt.Errorf("log file change: %w", err)
	}
	return nil
}

func (s *Store) CountPendingMessages(ctx context.Context, taskID string) (int, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM agent_messages
		WHERE task_id = ? AND status = ?`,
		taskID, string(domain.MessageStatusPending),
	)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count pending messages: %w", err)
	}
	return count, nil
}

func (s *Store) CountActiveMessages(ctx context.Context, taskID string) (int, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*)
		FROM agent_messages m
		LEFT JOIN message_acks a
			ON a.message_id = m.id
			AND a.agent_id = m.to_agent
		WHERE m.task_id = ?
			AND (
				m.status = ?
				OR (m.status = ? AND a.id IS NULL)
			)`,
		taskID,
		string(domain.MessageStatusPending),
		string(domain.MessageStatusDelivered),
	)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count active messages: %w", err)
	}
	return count, nil
}

func (s *Store) ListTaskMessages(ctx context.Context, taskID string, limit int) ([]domain.Message, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, task_id, from_agent, to_agent, type, payload, correlation_id, idempotency_key,
			status, attempts, next_attempt_at, last_error, created_at
		FROM agent_messages
		WHERE task_id = ?
		ORDER BY created_at DESC
		LIMIT ?`,
		taskID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list task messages: %w", err)
	}
	defer rows.Close()

	result := make([]domain.Message, 0, limit)
	for rows.Next() {
		var m domain.Message
		var typ string
		var status string
		var payload string
		var nextAttemptAt int64
		var createdAt int64
		if err := rows.Scan(
			&m.ID, &m.TaskID, &m.FromAgent, &m.ToAgent, &typ, &payload, &m.CorrelationID, &m.IdempotencyKey,
			&status, &m.Attempts, &nextAttemptAt, &m.LastError, &createdAt,
		); err != nil {
			return nil, fmt.Errorf("scan task message: %w", err)
		}
		m.Type = domain.MessageType(typ)
		m.Payload = []byte(payload)
		m.Status = domain.MessageStatus(status)
		m.NextAttemptAt = unixToTime(nextAttemptAt)
		m.CreatedAt = unixToTime(createdAt)
		result = append(result, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task messages: %w", err)
	}
	return result, nil
}

func (s *Store) ListTaskMessageAcks(ctx context.Context, taskID string, limit int) ([]domain.MessageAck, error) {
	if limit <= 0 {
		limit = 400
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT a.id, a.message_id, a.agent_id, a.result, a.ack_at
		FROM message_acks a
		JOIN agent_messages m ON m.id = a.message_id
		WHERE m.task_id = ?
		ORDER BY a.ack_at DESC
		LIMIT ?`,
		taskID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list task acks: %w", err)
	}
	defer rows.Close()

	result := make([]domain.MessageAck, 0, limit)
	for rows.Next() {
		var item domain.MessageAck
		var ackAt int64
		if err := rows.Scan(&item.ID, &item.MessageID, &item.AgentID, &item.Result, &ackAt); err != nil {
			return nil, fmt.Errorf("scan task ack: %w", err)
		}
		item.AckAt = unixToTime(ackAt)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task acks: %w", err)
	}
	return result, nil
}

func (s *Store) ListTaskDecisions(ctx context.Context, taskID string, limit int) ([]domain.DecisionLog, error) {
	if limit <= 0 {
		limit = 300
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT id, task_id, actor, action, reason, payload, created_at
		FROM decision_log
		WHERE task_id = ?
		ORDER BY created_at DESC
		LIMIT ?`,
		taskID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list task decisions: %w", err)
	}
	defer rows.Close()

	result := make([]domain.DecisionLog, 0, limit)
	for rows.Next() {
		var item domain.DecisionLog
		var payload string
		var createdAt int64
		if err := rows.Scan(&item.ID, &item.TaskID, &item.Actor, &item.Action, &item.Reason, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("scan decision: %w", err)
		}
		item.Payload = []byte(payload)
		item.CreatedAt = unixToTime(createdAt)
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate decisions: %w", err)
	}
	return result, nil
}

func int64ToTimePtr(v sql.NullInt64) *time.Time {
	if !v.Valid || v.Int64 <= 0 {
		return nil
	}
	t := time.Unix(v.Int64, 0).UTC()
	return &t
}

func unixToTime(v int64) time.Time {
	return time.Unix(v, 0).UTC()
}

func nullableUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Unix()
}

func parseAllowedTypes(raw string) ([]domain.MessageType, error) {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil, err
	}
	result := make([]domain.MessageType, 0, len(values))
	for _, v := range values {
		result = append(result, domain.MessageType(v))
	}
	return result, nil
}

func messageTypeAllowed(msgType domain.MessageType, allowed []domain.MessageType) bool {
	for _, item := range allowed {
		if item == msgType || item == domain.MessageType("*") {
			return true
		}
	}
	return false
}

func normalizeRelPath(p string) string {
	cleaned := strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	cleaned = strings.TrimPrefix(cleaned, "./")
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" {
		return "."
	}
	return cleaned
}

func globMatch(pattern string, value string) bool {
	p := normalizeRelPath(pattern)
	v := normalizeRelPath(value)
	if p == "**" || p == "*" {
		return true
	}
	if strings.HasSuffix(p, "/**") {
		base := strings.TrimSuffix(p, "/**")
		return v == base || strings.HasPrefix(v, base+"/")
	}
	ok, err := path.Match(p, v)
	if err != nil {
		return false
	}
	return ok
}
