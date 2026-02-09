-- organ_codex initial schema
-- SQLite

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
