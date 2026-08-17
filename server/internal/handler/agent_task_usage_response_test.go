package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestListAgentTasksHydratesUsage pins the JSON contract used by
// `multica agent tasks --output json`: usage is returned at the stored
// (provider, model) grain, only for tasks owned by the requested agent, and
// remains absent when a task has no recorded usage.
func TestListAgentTasksHydratesUsage(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()

	agentID := createHandlerTestAgent(t, "AgentTaskUsage", []byte("[]"))
	otherAgentID := createHandlerTestAgent(t, "AgentTaskUsageOther", []byte("[]"))

	newTask := func(agentID string) string {
		var id string
		if err := testPool.QueryRow(ctx, `
			INSERT INTO agent_task_queue (agent_id, runtime_id, status, priority)
			VALUES ($1, (SELECT runtime_id FROM agent WHERE id = $1), 'completed', 0)
			RETURNING id
		`, agentID).Scan(&id); err != nil {
			t.Fatalf("create task: %v", err)
		}
		t.Cleanup(func() {
			testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE id = $1`, id)
		})
		return id
	}

	usageTask := newTask(agentID)
	noUsageTask := newTask(agentID)
	otherAgentTask := newTask(otherAgentID)

	if _, err := testPool.Exec(ctx, `
		INSERT INTO task_usage (
			task_id, provider, model, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, cost_usd_ticks
		)
		VALUES ($1, 'anthropic', 'claude-opus-5', 96000, 34000, 712000, 50000, NULL),
		       ($1, 'openai', 'gpt-5.6-terra', 31000, 12000, 158000, 11000, 3310000000),
		       ($2, 'openai', 'other-agent-model', 1, 2, 3, 4, 5)
	`, usageTask, otherAgentTask); err != nil {
		t.Fatalf("insert task usage: %v", err)
	}

	usageRows, err := testHandler.Queries.ListAgentTaskUsage(ctx, parseUUID(agentID))
	if err != nil {
		t.Fatalf("list agent task usage: %v", err)
	}
	if len(usageRows) != 2 {
		t.Fatalf("agent-scoped usage rows = %d, want 2: %+v", len(usageRows), usageRows)
	}
	for _, row := range usageRows {
		if uuidToString(row.TaskID) != usageTask {
			t.Fatalf("agent-scoped usage leaked task %s, want only %s", uuidToString(row.TaskID), usageTask)
		}
	}

	req := newRequest(http.MethodGet, "/api/agents/"+agentID+"/tasks", nil)
	req = withURLParam(req, "id", agentID)
	w := httptest.NewRecorder()
	testHandler.ListAgentTasks(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp []AgentTaskResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode task list: %v", err)
	}

	byID := make(map[string]AgentTaskResponse, len(resp))
	for _, task := range resp {
		byID[task.ID] = task
	}
	if _, ok := byID[otherAgentTask]; ok {
		t.Fatalf("response leaked task %s from another agent", otherAgentTask)
	}

	withUsage, ok := byID[usageTask]
	if !ok {
		t.Fatalf("usage task %s missing from response", usageTask)
	}
	if len(withUsage.Usage) != 2 {
		t.Fatalf("usage rows = %d, want 2: %+v", len(withUsage.Usage), withUsage.Usage)
	}

	byModel := make(map[string]TaskUsageData, len(withUsage.Usage))
	for _, usage := range withUsage.Usage {
		byModel[usage.Model] = usage
	}
	opus, ok := byModel["claude-opus-5"]
	if !ok {
		t.Fatalf("claude-opus-5 row missing: %+v", withUsage.Usage)
	}
	if opus.Provider != "anthropic" || opus.InputTokens != 96000 ||
		opus.OutputTokens != 34000 || opus.CacheReadTokens != 712000 ||
		opus.CacheWriteTokens != 50000 {
		t.Errorf("unexpected claude-opus-5 usage: %+v", opus)
	}
	if opus.CostUsdTicks != nil {
		t.Errorf("claude-opus-5 cost_usd_ticks = %v, want nil", opus.CostUsdTicks)
	}

	terra, ok := byModel["gpt-5.6-terra"]
	if !ok {
		t.Fatalf("gpt-5.6-terra row missing: %+v", withUsage.Usage)
	}
	if terra.CostUsdTicks == nil || *terra.CostUsdTicks != 3310000000 {
		t.Errorf("gpt-5.6-terra cost_usd_ticks = %v, want 3310000000", terra.CostUsdTicks)
	}

	withoutUsage, ok := byID[noUsageTask]
	if !ok {
		t.Fatalf("no-usage task %s missing from response", noUsageTask)
	}
	if len(withoutUsage.Usage) != 0 {
		t.Errorf("task with no recorded usage has %d rows: %+v", len(withoutUsage.Usage), withoutUsage.Usage)
	}

	var raw []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw task list: %v", err)
	}
	for _, task := range raw {
		if task["id"] != noUsageTask {
			continue
		}
		if _, present := task["usage"]; present {
			t.Errorf("no-usage task serialises a usage key: %v", task["usage"])
		}
	}
}
