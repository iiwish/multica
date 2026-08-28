CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_agent_task_queue_agent_created_at_keyset
    ON agent_task_queue (agent_id, created_at DESC, id DESC);
