package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
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
		return dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id": handlerTestRuntimeID(t),
			"status":     "completed",
		})
	}

	usageTask := newTask(agentID)
	noUsageTask := newTask(agentID)
	otherAgentTask := newTask(otherAgentID)

	dbfx.Insert(t, "task_usage", testutil.Cols{
		"task_id":            usageTask,
		"provider":           "anthropic",
		"model":              "claude-opus-5",
		"input_tokens":       96000,
		"output_tokens":      34000,
		"cache_read_tokens":  712000,
		"cache_write_tokens": 50000,
		"cost_usd_ticks":     nil,
	})
	dbfx.Insert(t, "task_usage", testutil.Cols{
		"task_id":            usageTask,
		"provider":           "openai",
		"model":              "gpt-5.6-terra",
		"input_tokens":       31000,
		"output_tokens":      12000,
		"cache_read_tokens":  158000,
		"cache_write_tokens": 11000,
		"cost_usd_ticks":     int64(3310000000),
	})
	dbfx.Insert(t, "task_usage", testutil.Cols{
		"task_id":            otherAgentTask,
		"provider":           "openai",
		"model":              "other-agent-model",
		"input_tokens":       1,
		"output_tokens":      2,
		"cache_read_tokens":  3,
		"cache_write_tokens": 4,
		"cost_usd_ticks":     5,
	})

	usageRows, err := testHandler.Queries.ListAgentTaskUsage(ctx, db.ListAgentTaskUsageParams{
		AgentID: parseUUID(agentID),
		TaskIds: []pgtype.UUID{parseUUID(usageTask), parseUUID(noUsageTask)},
	})
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
	var resp []AgentTaskResponse
	w := testutil.Call(t, testHandler.ListAgentTasks, req).Want(http.StatusOK).JSON(&resp)

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

func TestListAgentTasksFiltersAndLimitsUsageScope(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	agentID := createHandlerTestAgent(t, "AgentTaskUsageWindow", []byte("[]"))
	newTask := func() string {
		return dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id": handlerTestRuntimeID(t),
			"status":     "completed",
		})
	}
	oldTask := newTask()
	matchingTask := newTask()
	newTaskID := newTask()

	createdAt := map[string]string{
		oldTask:      "2026-08-27T00:00:00Z",
		matchingTask: "2026-08-27T02:00:00Z",
		newTaskID:    "2026-08-27T04:00:00Z",
	}
	for taskID, timestamp := range createdAt {
		if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET created_at = $2::timestamptz WHERE id = $1`, taskID, timestamp); err != nil {
			t.Fatalf("set task created_at: %v", err)
		}
	}
	for _, taskID := range []string{matchingTask, newTaskID} {
		dbfx.Insert(t, "task_usage", testutil.Cols{
			"task_id":           taskID,
			"provider":          "openai",
			"model":             "gpt-5.6-terra",
			"input_tokens":      10,
			"output_tokens":     5,
			"cache_read_tokens": 2,
		})
	}

	req := newRequest(http.MethodGet, "/api/agents/"+agentID+"/tasks?since=2026-08-27T01%3A00%3A00Z&until=2026-08-27T03%3A00%3A00Z&limit=1", nil)
	req = withURLParam(req, "id", agentID)
	var resp []AgentTaskResponse
	testutil.Call(t, testHandler.ListAgentTasks, req).Want(http.StatusOK).JSON(&resp)
	if len(resp) != 1 || resp[0].ID != matchingTask {
		t.Fatalf("filtered tasks = %+v, want only %s", resp, matchingTask)
	}
	if len(resp[0].Usage) != 1 || resp[0].Usage[0].InputTokens != 10 {
		t.Fatalf("filtered task usage = %+v", resp[0].Usage)
	}
}

func TestListAgentTasksPaginatesEqualCreatedAtWithoutSkipping(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	agentID := createHandlerTestAgent(t, "AgentTaskPagination", []byte("[]"))
	taskIDs := make([]string, 0, 3)
	for range 3 {
		taskID := dbfx.Task(t, agentID, testutil.Cols{
			"runtime_id": handlerTestRuntimeID(t),
			"status":     "completed",
		})
		if _, err := testPool.Exec(context.Background(), `UPDATE agent_task_queue SET created_at = '2026-08-27T02:00:00Z'::timestamptz WHERE id = $1`, taskID); err != nil {
			t.Fatalf("set task created_at: %v", err)
		}
		taskIDs = append(taskIDs, taskID)
	}

	want := append([]string(nil), taskIDs...)
	sort.Sort(sort.Reverse(sort.StringSlice(want)))
	got := make([]string, 0, len(taskIDs))
	query := "limit=1"
	for page := range len(taskIDs) {
		req := newRequest(http.MethodGet, "/api/agents/"+agentID+"/tasks?"+query, nil)
		req = withURLParam(req, "id", agentID)
		var tasks []AgentTaskResponse
		resp := testutil.Call(t, testHandler.ListAgentTasks, req).Want(http.StatusOK).JSON(&tasks)
		if len(tasks) != 1 {
			t.Fatalf("page %d tasks = %d, want 1: %+v", page+1, len(tasks), tasks)
		}
		got = append(got, tasks[0].ID)

		hasMore := page < len(taskIDs)-1
		if gotHeader := resp.Header().Get(agentTaskHasMoreHeader); gotHeader != strconv.FormatBool(hasMore) {
			t.Fatalf("page %d %s = %q, want %t", page+1, agentTaskHasMoreHeader, gotHeader, hasMore)
		}
		if !hasMore {
			continue
		}
		before := resp.Header().Get(agentTaskNextBeforeHeader)
		beforeID := resp.Header().Get(agentTaskNextBeforeIDHeader)
		if before == "" || beforeID == "" {
			t.Fatalf("page %d missing pagination cursor headers", page+1)
		}
		query = url.Values{
			"limit":     {"1"},
			"before":    {before},
			"before_id": {beforeID},
		}.Encode()
	}

	if !slices.Equal(got, want) {
		t.Fatalf("paginated task ids = %v, want %v", got, want)
	}
}

func TestParseAgentTaskListParamsRejectsInvalidBounds(t *testing.T) {
	params, pageLimit, err := parseAgentTaskListParams(newRequest(http.MethodGet, "/api/agents/example/tasks", nil), pgtype.UUID{})
	if err != nil {
		t.Fatalf("default params: %v", err)
	}
	if params.LimitRows.Valid || pageLimit != 0 {
		t.Fatalf("default pagination = (%+v, %d), want unbounded", params.LimitRows, pageLimit)
	}

	tests := []struct {
		query string
		want  string
	}{
		{"since=not-a-time", "since must be an RFC3339 timestamp"},
		{"until=not-a-time", "until must be an RFC3339 timestamp"},
		{"since=2026-08-28T02%3A00%3A00Z&until=2026-08-28T01%3A00%3A00Z", "since must be earlier than until"},
		{"before=2026-08-28T02%3A00%3A00Z", "before and before_id must be set together"},
		{"before_id=00000000-0000-0000-0000-000000000001", "before and before_id must be set together"},
		{"before=2026-08-28T02%3A00%3A00Z&before_id=not-a-uuid", "before_id must be a UUID"},
		{"limit=0", "limit must be between 1 and 1000"},
		{"limit=1001", "limit must be between 1 and 1000"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			req := newRequest(http.MethodGet, "/api/agents/example/tasks?"+tt.query, nil)
			_, _, err := parseAgentTaskListParams(req, pgtype.UUID{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestListAgentTasksReturnsErrorWhenUsageLoadFails(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}

	agentID := createHandlerTestAgent(t, "AgentTaskUsageFailure", []byte("[]"))
	dbfx.Task(t, agentID, testutil.Cols{
		"runtime_id": handlerTestRuntimeID(t),
		"status":     "completed",
	})

	lockTx, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin controlled task usage query failure: %v", err)
	}
	t.Cleanup(func() { _ = lockTx.Rollback(context.Background()) })
	if _, err := lockTx.Exec(context.Background(), `LOCK TABLE task_usage IN ACCESS EXCLUSIVE MODE`); err != nil {
		_ = lockTx.Rollback(context.Background())
		t.Fatalf("lock task usage for controlled query failure: %v", err)
	}

	req := newRequest(http.MethodGet, "/api/agents/"+agentID+"/tasks", nil)
	req = withURLParam(req, "id", agentID)
	errorCtx, cancel := context.WithTimeout(req.Context(), 250*time.Millisecond)
	req = req.WithContext(errorCtx)
	w := testutil.Call(t, testHandler.ListAgentTasks, req)
	cancel()
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release controlled task usage lock: %v", err)
	}

	w.Want(http.StatusInternalServerError)
	if !strings.Contains(w.Body.String(), "failed to list agent task usage") {
		t.Fatalf("usage query failure should not fall back to empty usage: %s", w.Body.String())
	}
}
