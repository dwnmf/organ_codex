package domain

import (
	"encoding/json"
	"time"
)

type TaskStatus string

const (
	TaskStatusPlanned  TaskStatus = "planned"
	TaskStatusRunning  TaskStatus = "running"
	TaskStatusBlocked  TaskStatus = "blocked"
	TaskStatusDone     TaskStatus = "done"
	TaskStatusFailed   TaskStatus = "failed"
	TaskStatusCanceled TaskStatus = "canceled"
)

type MessageType string

const (
	MessageTypeRequest  MessageType = "REQUEST"
	MessageTypePropose  MessageType = "PROPOSE"
	MessageTypeReview   MessageType = "REVIEW"
	MessageTypeBlocked  MessageType = "BLOCKED"
	MessageTypeDone     MessageType = "DONE"
	MessageTypeEscalate MessageType = "ESCALATE"
)

type MessageStatus string

const (
	MessageStatusPending     MessageStatus = "pending"
	MessageStatusDispatching MessageStatus = "dispatching"
	MessageStatusDelivered   MessageStatus = "delivered"
	MessageStatusAcked       MessageStatus = "acked"
	MessageStatusFailed      MessageStatus = "failed"
)

type PermissionEffect string

const (
	PermissionEffectAllow PermissionEffect = "allow"
	PermissionEffectDeny  PermissionEffect = "deny"
)

type FileOperation string

const (
	FileOperationRead   FileOperation = "read"
	FileOperationWrite  FileOperation = "write"
	FileOperationCreate FileOperation = "create"
	FileOperationDelete FileOperation = "delete"
	FileOperationRename FileOperation = "rename"
)

type Task struct {
	ID           string     `json:"id"`
	Goal         string     `json:"goal"`
	Scope        string     `json:"scope"`
	OwnerAgent   string     `json:"owner_agent"`
	Status       TaskStatus `json:"status"`
	Priority     int        `json:"priority"`
	DeadlineAt   *time.Time `json:"deadline_at,omitempty"`
	BudgetTokens int        `json:"budget_tokens"`
	MaxHops      int        `json:"max_hops"`
	HopCount     int        `json:"hop_count"`
	LastError    string     `json:"last_error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

type Message struct {
	ID             string          `json:"id"`
	TaskID         string          `json:"task_id"`
	FromAgent      string          `json:"from_agent"`
	ToAgent        string          `json:"to_agent"`
	Type           MessageType     `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	CorrelationID  string          `json:"correlation_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Status         MessageStatus   `json:"status"`
	Attempts       int             `json:"attempts"`
	NextAttemptAt  time.Time       `json:"next_attempt_at"`
	LastError      string          `json:"last_error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

type AgentChannel struct {
	ID           int64         `json:"id"`
	TaskID       string        `json:"task_id"`
	FromAgent    string        `json:"from_agent"`
	ToAgent      string        `json:"to_agent"`
	AllowedTypes []MessageType `json:"allowed_types"`
	MaxMsgs      int           `json:"max_msgs"`
	SentMsgs     int           `json:"sent_msgs"`
	ExpiresAt    *time.Time    `json:"expires_at,omitempty"`
}

type TaskPermission struct {
	ID          int64            `json:"id"`
	TaskID      string           `json:"task_id"`
	AgentID     string           `json:"agent_id"`
	Effect      PermissionEffect `json:"effect"`
	Operation   FileOperation    `json:"operation"`
	PathPattern string           `json:"path_pattern"`
	ExpiresAt   *time.Time       `json:"expires_at,omitempty"`
}

type Artifact struct {
	ID            string          `json:"id"`
	TaskID        string          `json:"task_id"`
	ProducerAgent string          `json:"producer_agent"`
	Kind          string          `json:"kind"`
	URI           string          `json:"uri"`
	Checksum      string          `json:"checksum"`
	Metadata      json.RawMessage `json:"metadata"`
	CreatedAt     time.Time       `json:"created_at"`
}

type DecisionLog struct {
	ID        int64           `json:"id"`
	TaskID    string          `json:"task_id"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Reason    string          `json:"reason"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

type FileChangeLog struct {
	ID        int64         `json:"id"`
	TaskID    string        `json:"task_id"`
	AgentID   string        `json:"agent_id"`
	Operation FileOperation `json:"operation"`
	Path      string        `json:"path"`
	Allowed   bool          `json:"allowed"`
	Reason    string        `json:"reason"`
	CreatedAt time.Time     `json:"created_at"`
}

type MessageAck struct {
	ID        int64     `json:"id"`
	MessageID string    `json:"message_id"`
	AgentID   string    `json:"agent_id"`
	Result    string    `json:"result"`
	AckAt     time.Time `json:"ack_at"`
}

type TimelineEvent struct {
	Kind      string          `json:"kind"`
	TaskID    string          `json:"task_id"`
	RefID     string          `json:"ref_id"`
	Actor     string          `json:"actor,omitempty"`
	FromAgent string          `json:"from_agent,omitempty"`
	ToAgent   string          `json:"to_agent,omitempty"`
	Type      string          `json:"type,omitempty"`
	Status    string          `json:"status,omitempty"`
	Action    string          `json:"action,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Result    string          `json:"result,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type TaskRequestPayload struct {
	Goal               string   `json:"goal"`
	Scope              string   `json:"scope"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
	TargetPaths        []string `json:"target_paths,omitempty"`
}

type WorkRequestPayload struct {
	NodeID             string   `json:"node_id,omitempty"`
	Goal               string   `json:"goal"`
	Scope              string   `json:"scope"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
	TargetPaths        []string `json:"target_paths,omitempty"`
}

type WorkResultPayload struct {
	NodeID       string   `json:"node_id,omitempty"`
	Summary      string   `json:"summary"`
	CreatedFiles []string `json:"created_files,omitempty"`
}

type PlanProposalPayload struct {
	Summary string     `json:"summary,omitempty"`
	Nodes   []PlanNode `json:"nodes"`
}

type PlanNode struct {
	ID        string      `json:"id"`
	AgentID   string      `json:"agent_id"`
	Type      MessageType `json:"type"`
	Goal      string      `json:"goal,omitempty"`
	Scope     string      `json:"scope,omitempty"`
	DependsOn []string    `json:"depends_on,omitempty"`
}

type ReviewRequestPayload struct {
	NodeID       string   `json:"node_id"`
	Summary      string   `json:"summary"`
	CreatedFiles []string `json:"created_files"`
}
