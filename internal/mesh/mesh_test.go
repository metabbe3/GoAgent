package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/metabbe3/GoAgent/internal/guardrail"
)

func TestMeshHappyPathRegisterAssignReport(t *testing.T) {
	tmpDir := t.TempDir()
	token := "mesh-secret-123"

	q := NewQueue(tmpDir, 10*time.Second, 60*time.Second)
	srv := NewServer("127.0.0.1:0", token, q)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = srv.Stop(stopCtx)
	}()

	leaderURL := "http://" + srv.Addr()

	// Add mission on leader
	mission, err := q.AddMission("Analyze cluster logs", []SubtaskInput{
		{Goal: "Inspect auth logs", Capability: "logs"},
	})
	if err != nil {
		t.Fatalf("add mission failed: %v", err)
	}

	executed := make(chan string, 1)
	client := NewClient(leaderURL, token, "worker-1", []string{"logs"}, func(ctx context.Context, goal string) (string, error) {
		executed <- goal
		return "Found 0 critical errors", nil
	})
	defer client.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Start(ctx)
	}()

	select {
	case goal := <-executed:
		if goal != "Inspect auth logs" {
			t.Errorf("unexpected goal executed: %s", goal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for task execution")
	}

	// Verify report landed on leader
	time.Sleep(200 * time.Millisecond)
	status, err := q.MissionStatus(mission.ID)
	if err != nil {
		t.Fatalf("mission status failed: %v", err)
	}
	if status.Status != "completed" {
		t.Errorf("expected mission completed, got: %s", status.Status)
	}
	if status.Subtasks[0].Result != "Found 0 critical errors" {
		t.Errorf("unexpected subtask result: %s", status.Subtasks[0].Result)
	}
}

func TestMeshAuthRejectWithoutToken(t *testing.T) {
	token := "correct-token"
	q := NewQueue("", 10*time.Second, 60*time.Second)
	srv := NewServer("127.0.0.1:0", token, q)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())

	tests := []struct {
		name       string
		token      string
		wantStatus int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"wrong token", "wrong-secret", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{}
			req, _ := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/nexttask?id=w1", nil)
			if tt.token != "" {
				req.Header.Set(HeaderMeshToken, tt.token)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("expected status %d, got %d", tt.wantStatus, resp.StatusCode)
			}
		})
	}
}

func TestLongPollReleaseWhenTaskArrives(t *testing.T) {
	token := "poll-token"
	q := NewQueue("", 10*time.Second, 60*time.Second)
	srv := NewServer("127.0.0.1:0", token, q)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())

	leaderURL := "http://" + srv.Addr()
	taskReceived := make(chan *Task, 1)

	client := NewClient(leaderURL, token, "waiter-worker", []string{"shell"}, func(ctx context.Context, goal string) (string, error) {
		return "ok", nil
	})

	// Register worker first
	if err := client.registerOnce(context.Background()); err != nil {
		t.Fatalf("register failed: %v", err)
	}

	// Start long-poll in background
	go func() {
		task, err := client.fetchNextTask(context.Background())
		if err == nil && task != nil {
			taskReceived <- task
		}
	}()

	// Wait briefly to ensure worker is waiting on long-poll
	time.Sleep(150 * time.Millisecond)

	// Now add task to leader
	start := time.Now()
	_, err := q.AddMission("Delayed mission", []SubtaskInput{
		{Goal: "Run diagnostic", Capability: "shell"},
	})
	if err != nil {
		t.Fatalf("add mission failed: %v", err)
	}

	select {
	case task := <-taskReceived:
		elapsed := time.Since(start)
		if task.Goal != "Run diagnostic" {
			t.Errorf("unexpected task goal: %s", task.Goal)
		}
		if elapsed > 3*time.Second {
			t.Errorf("long-poll took too long to unblock: %v", elapsed)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("long-poll was not released when task arrived")
	}
}

func TestDeadWorkerRequeue(t *testing.T) {
	q := NewQueue("", 10*time.Second, 60*time.Second)

	// Register worker-1
	w1 := q.RegisterWorker("worker-1", []string{"logs"})

	// Add mission with 1 subtask
	_, err := q.AddMission("Check node health", []SubtaskInput{
		{Goal: "Inspect node 1", Capability: "logs"},
	})
	if err != nil {
		t.Fatalf("add mission failed: %v", err)
	}

	// Worker 1 claims task
	task, err := q.GetNextTask("worker-1", time.Second)
	if err != nil || task == nil {
		t.Fatalf("worker-1 failed to get task: %v", err)
	}
	if len(q.inFlightTasks) != 1 {
		t.Fatalf("expected 1 task in flight, got %d", len(q.inFlightTasks))
	}

	// Mock worker 1 last seen 100 seconds ago (>90s dead threshold)
	w1.LastSeen = time.Now().Add(-100 * time.Second)

	requeued := q.SweepDeadWorkers(90 * time.Second)
	if requeued != 1 {
		t.Fatalf("expected 1 task requeued, got %d", requeued)
	}
	if w1.Status != "dead" {
		t.Errorf("expected worker-1 to be marked dead, got %s", w1.Status)
	}
	if len(q.pendingTasks) != 1 {
		t.Errorf("expected requeued task in pending, got %d", len(q.pendingTasks))
	}

	// Worker 2 should now be able to claim the requeued task
	q.RegisterWorker("worker-2", []string{"logs"})
	task2, err := q.GetNextTask("worker-2", time.Second)
	if err != nil || task2 == nil || task2.ID != task.ID {
		t.Fatalf("worker-2 should have claimed requeued task %s, got %v (err: %v)", task.ID, task2, err)
	}
}

func TestCapabilityRoutingAndRoundRobin(t *testing.T) {
	q := NewQueue("", 10*time.Second, 60*time.Second)

	q.RegisterWorker("w-logs", []string{"logs"})
	q.RegisterWorker("w-shell-1", []string{"shell"})
	q.RegisterWorker("w-shell-2", []string{"shell"})

	// Task requiring shell
	_, _ = q.AddMission("Run shell jobs", []SubtaskInput{
		{Goal: "Shell job 1", Capability: "shell"},
		{Goal: "Shell job 2", Capability: "shell"},
	})

	// Worker w-logs should NOT receive shell tasks
	taskLogs, err := q.GetNextTask("w-logs", 50*time.Millisecond)
	if err != nil || taskLogs != nil {
		t.Fatalf("w-logs should not receive shell task, got: %v (err: %v)", taskLogs, err)
	}

	// Worker w-shell-1 claims first task
	task1, err := q.GetNextTask("w-shell-1", time.Second)
	if err != nil || task1 == nil || task1.Goal != "Shell job 1" {
		t.Fatalf("w-shell-1 expected Shell job 1, got %v (err: %v)", task1, err)
	}

	// Worker w-shell-2 claims second task
	task2, err := q.GetNextTask("w-shell-2", time.Second)
	if err != nil || task2 == nil || task2.Goal != "Shell job 2" {
		t.Fatalf("w-shell-2 expected Shell job 2, got %v (err: %v)", task2, err)
	}
}

func TestJSONLReplay(t *testing.T) {
	tmpDir := t.TempDir()

	q1 := NewQueue(tmpDir, 10*time.Second, 60*time.Second)
	m, err := q1.AddMission("Replay test mission", []SubtaskInput{
		{Goal: "Subtask 1", Capability: "general"},
		{Goal: "Subtask 2", Capability: "general"},
	})
	if err != nil {
		t.Fatalf("failed to add mission: %v", err)
	}

	// Worker claims subtask 1 and reports it completed
	q1.RegisterWorker("w1", []string{"general"})
	task1, _ := q1.GetNextTask("w1", time.Second)
	_ = q1.ReportTask(ReportReq{
		WorkerID:  "w1",
		TaskID:    task1.ID,
		MissionID: m.ID,
		Success:   true,
		Result:    "Subtask 1 finished",
	})

	// Subtask 2 was assigned but not reported (simulating mid-task server restart)
	task2, _ := q1.GetNextTask("w1", time.Second)
	if task2 == nil {
		t.Fatal("expected task2 to be assigned")
	}

	// Now create new queue pointing to same directory: state should replay
	q2 := NewQueue(tmpDir, 10*time.Second, 60*time.Second)

	status, err := q2.MissionStatus(m.ID)
	if err != nil {
		t.Fatalf("failed to read replayed mission: %v", err)
	}
	if status.Text != "Replay test mission" {
		t.Errorf("unexpected replayed mission text: %s", status.Text)
	}

	// Subtask 1 was completed
	if status.Subtasks[0].Status != "completed" || status.Subtasks[0].Result != "Subtask 1 finished" {
		t.Errorf("subtask 1 replay state incorrect: %+v", status.Subtasks[0])
	}

	// Subtask 2 was in-flight at restart, so it should be restored to pending for re-execution
	if len(q2.pendingTasks) != 1 || q2.pendingTasks[0].ID != task2.ID {
		t.Errorf("expected in-flight task to be restored to pending queue after restart, got %d pending", len(q2.pendingTasks))
	}
}

func TestReportRedaction(t *testing.T) {
	reportChan := make(chan string, 1)
	var servedTask bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rpc" {
			var req RPCRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Method == "mesh.register" {
				_ = json.NewEncoder(w).Encode(RPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result:  RegisterResp{WorkerID: "w-redact", HeartbeatSec: 30, Poll: "/nexttask"},
				})
				return
			}
			if req.Method == "mesh.report" {
				var report ReportReq
				_ = json.Unmarshal(req.Params, &report)
				reportChan <- report.Result
				_ = json.NewEncoder(w).Encode(RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: ReportResp{OK: true}})
				return
			}
		}
		if r.URL.Path == "/nexttask" {
			if !servedTask {
				servedTask = true
				task := Task{
					ID:        "task-secret",
					MissionID: "mission-secret",
					Goal:      "extract credentials",
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(task)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// Worker callback returns a planted secret API key
	apiKey := "sk-proj-supersecretkey1234567890abcdef"
	client := NewClient(srv.URL, "token", "w-redact", nil, func(ctx context.Context, goal string) (string, error) {
		return "Found token: " + apiKey, nil
	})
	defer client.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		_ = client.Start(ctx)
	}()

	select {
	case reportedResult := <-reportChan:
		if strings.Contains(reportedResult, apiKey) {
			t.Fatalf("API key leaked through report unredacted: %s", reportedResult)
		}
		if !strings.Contains(reportedResult, "[REDACTED") {
			t.Fatalf("expected [REDACTED in report, got: %s", reportedResult)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not report task in time")
	}
}

func TestClientBackoff(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			http.Error(w, "server busy", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RPCResponse{
			JSONRPC: "2.0",
			ID:      1,
			Result:  RegisterResp{WorkerID: "w-backoff", HeartbeatSec: 30, Poll: "/nexttask"},
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "token", "w-backoff", nil, nil)
	client.backoffBase = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := client.registerWithRetry(ctx)
	if err != nil {
		t.Fatalf("register with retry failed: %v", err)
	}
	if attempts < 3 {
		t.Errorf("expected at least 3 attempts with backoff, got %d", attempts)
	}
}

func TestDirectGuardrailRedact(t *testing.T) {
	redacted, _ := guardrail.Redact("My API key is sk-proj-12345678901234567890123456789012")
	if strings.Contains(redacted, "sk-proj-12345678901234567890123456789012") {
		t.Errorf("redaction failed")
	}
}

func TestSweepTaskTimeoutsRequeuesThenFails(t *testing.T) {
	q := NewQueue("", 5*time.Second, 60*time.Second, 2) // maxSubtaskAttempts = 2

	_ = q.RegisterWorker("w1", []string{"general"})
	m, err := q.AddMission("Timeout test", []SubtaskInput{
		{Goal: "Slow subtask", Capability: "general"},
	})
	if err != nil {
		t.Fatalf("AddMission failed: %v", err)
	}

	// 1st assignment
	task1, err := q.GetNextTask("w1", time.Second)
	if err != nil || task1 == nil {
		t.Fatalf("expected task assigned, got: %v (err: %v)", task1, err)
	}
	if task1.Attempts != 1 {
		t.Errorf("expected Attempts 1, got %d", task1.Attempts)
	}

	// Manipulate AssignedAt to 10s in the past
	q.mu.Lock()
	task1.AssignedAt = time.Now().Add(-10 * time.Second)
	q.mu.Unlock()

	// 1st Sweep: Attempts (1) < MaxSubtaskAttempts (2) -> should requeue (attempt 1/2)
	swept := q.SweepTaskTimeouts()
	if swept != 1 {
		t.Fatalf("expected 1 task swept, got %d", swept)
	}

	status1, _ := q.MissionStatus(m.ID)
	if status1.Subtasks[0].Status != "pending" {
		t.Errorf("expected subtask status pending after requeue, got: %s", status1.Subtasks[0].Status)
	}

	// 2nd assignment
	task2, err := q.GetNextTask("w1", time.Second)
	if err != nil || task2 == nil {
		t.Fatalf("expected task re-assigned, got: %v (err: %v)", task2, err)
	}
	if task2.Attempts != 2 {
		t.Errorf("expected Attempts 2, got %d", task2.Attempts)
	}

	// Manipulate AssignedAt to 10s in the past again
	q.mu.Lock()
	task2.AssignedAt = time.Now().Add(-10 * time.Second)
	q.mu.Unlock()

	// 2nd Sweep: Attempts (2) >= MaxSubtaskAttempts (2) -> should fail
	swept2 := q.SweepTaskTimeouts()
	if swept2 != 1 {
		t.Fatalf("expected 1 task swept, got %d", swept2)
	}

	status2, _ := q.MissionStatus(m.ID)
	if status2.Subtasks[0].Status != "failed" {
		t.Errorf("expected subtask status failed after attempts exhausted, got: %s", status2.Subtasks[0].Status)
	}
	if !strings.Contains(status2.Subtasks[0].Error, "task timed out after") {
		t.Errorf("expected timeout error message, got: %s", status2.Subtasks[0].Error)
	}
	if status2.Status != "failed" {
		t.Errorf("expected mission status failed, got: %s", status2.Status)
	}
}

func TestHeartbeatCarriesAbort(t *testing.T) {
	token := "token-abort"
	q := NewQueue("", 5*time.Second, 60*time.Second)
	srv := NewServer("127.0.0.1:0", token, q)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())

	q.RegisterWorker("w-abort", []string{"general"})
	_, err := q.AddMission("Heartbeat abort test", []SubtaskInput{
		{Goal: "Abort goal", Capability: "general"},
	})
	if err != nil {
		t.Fatalf("AddMission failed: %v", err)
	}

	task, err := q.GetNextTask("w-abort", time.Second)
	if err != nil || task == nil {
		t.Fatalf("failed to get task: %v", err)
	}

	// Mark AssignedAt old (past deadline)
	q.mu.Lock()
	task.AssignedAt = time.Now().Add(-10 * time.Second)
	q.mu.Unlock()

	// Client sends heartbeat
	client := NewClient("http://"+srv.Addr(), token, "w-abort", []string{"general"}, nil)
	params := HeartbeatReq{WorkerID: "w-abort"}
	rawParams, _ := json.Marshal(params)
	reqBody := RPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "mesh.heartbeat",
		Params:  rawParams,
	}

	var resp HeartbeatResp
	if err := client.callRPC(context.Background(), reqBody, &resp); err != nil {
		t.Fatalf("heartbeat RPC failed: %v", err)
	}

	if len(resp.AbortTasks) == 0 {
		t.Fatalf("expected abort_tasks in heartbeat response, got none")
	}
	found := false
	for _, id := range resp.AbortTasks {
		if id == task.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected task %s in abort_tasks, got: %v", task.ID, resp.AbortTasks)
	}
}

func TestWorkerAbortsOnLeaderOrder(t *testing.T) {
	reportChan := make(chan ReportReq, 1)
	var servedTask bool
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/rpc" {
			var req RPCRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Method == "mesh.register" {
				_ = json.NewEncoder(w).Encode(RPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result:  RegisterResp{WorkerID: "w-abort-test", HeartbeatSec: 30, Poll: "/nexttask"},
				})
				return
			}
			if req.Method == "mesh.report" {
				var rep ReportReq
				_ = json.Unmarshal(req.Params, &rep)
				reportChan <- rep
				_ = json.NewEncoder(w).Encode(RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: ReportResp{OK: true}})
				return
			}
		}
		if r.URL.Path == "/nexttask" {
			mu.Lock()
			done := servedTask
			servedTask = true
			mu.Unlock()

			if !done {
				task := Task{
					ID:         "task-to-abort",
					MissionID:  "m-abort",
					Goal:       "Stuck execution",
					TimeoutSec: 60,
				}
				_ = json.NewEncoder(w).Encode(task)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	runBlock := make(chan struct{})
	client := NewClient(srv.URL, "token", "w-abort-test", []string{"general"}, func(ctx context.Context, goal string) (string, error) {
		close(runBlock)
		<-ctx.Done()
		return "", ctx.Err()
	})
	defer client.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Start(ctx)
	}()

	select {
	case <-runBlock:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start executing task in time")
	}

	// Trigger abort via HandleAbortTasks (as heartbeat loop does)
	client.HandleAbortTasks([]string{"task-to-abort"})

	select {
	case rep := <-reportChan:
		if rep.Success {
			t.Errorf("expected report failure after abort, got success")
		}
		if rep.Error != "aborted by leader" {
			t.Errorf("expected error 'aborted by leader', got: %q", rep.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for aborted task report")
	}
}

func TestWorkerTaskCtxDeadline(t *testing.T) {
	reportChan := make(chan ReportReq, 1)
	var servedTask bool
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/rpc" {
			var req RPCRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Method == "mesh.register" {
				_ = json.NewEncoder(w).Encode(RPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Result:  RegisterResp{WorkerID: "w-deadline", HeartbeatSec: 30, Poll: "/nexttask"},
				})
				return
			}
			if req.Method == "mesh.report" {
				var rep ReportReq
				_ = json.Unmarshal(req.Params, &rep)
				reportChan <- rep
				_ = json.NewEncoder(w).Encode(RPCResponse{JSONRPC: "2.0", ID: req.ID, Result: ReportResp{OK: true}})
				return
			}
		}
		if r.URL.Path == "/nexttask" {
			mu.Lock()
			done := servedTask
			servedTask = true
			mu.Unlock()

			if !done {
				task := Task{
					ID:         "task-timeout",
					MissionID:  "m-timeout",
					Goal:       "Sleep 10s",
					TimeoutSec: 1, // 1 second deadline
				}
				_ = json.NewEncoder(w).Encode(task)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	start := time.Now()
	client := NewClient(srv.URL, "token", "w-deadline", []string{"general"}, func(ctx context.Context, goal string) (string, error) {
		select {
		case <-time.After(10 * time.Second):
			return "done", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	})
	defer client.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Start(ctx)
	}()

	select {
	case rep := <-reportChan:
		elapsed := time.Since(start)
		if elapsed > 3*time.Second {
			t.Errorf("expected deadline execution to finish in ~1s, took %v", elapsed)
		}
		if rep.Success {
			t.Errorf("expected task failure due to deadline exceeded, got success")
		}
		if !strings.Contains(rep.Error, "deadline exceeded") {
			t.Errorf("expected deadline exceeded error, got: %q", rep.Error)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for task deadline report")
	}
}

func TestSubtaskRetryOnLLMFailure(t *testing.T) {
	token := "token-retry"
	q := NewQueue("", 10*time.Second, 60*time.Second, 2)
	srv := NewServer("127.0.0.1:0", token, q)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		_ = srv.Stop(stopCtx)
	}()

	m, err := q.AddMission("Retry test mission", []SubtaskInput{
		{Goal: "Flaky subtask", Capability: "general"},
	})
	if err != nil {
		t.Fatalf("AddMission failed: %v", err)
	}

	var attempts int
	var mu sync.Mutex

	client := NewClient("http://"+srv.Addr(), token, "w-retry", []string{"general"}, func(ctx context.Context, goal string) (string, error) {
		mu.Lock()
		attempts++
		cur := attempts
		mu.Unlock()

		if cur == 1 {
			return "", fmt.Errorf("llm connection reset")
		}
		return "recovered output", nil
	})
	defer client.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = client.Start(ctx)
	}()

	// Wait for mission completion
	var finalStatus *MissionState
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		st, _ := q.MissionStatus(m.ID)
		if st.Status == "completed" {
			finalStatus = st
			cancel()
			break
		}
	}

	if finalStatus == nil {
		st, _ := q.MissionStatus(m.ID)
		t.Fatalf("mission did not complete: status=%s, subtask=%+v", st.Status, st.Subtasks[0])
	}

	if finalStatus.Subtasks[0].Attempts != 2 {
		t.Errorf("expected Attempts 2 on subtask, got %d", finalStatus.Subtasks[0].Attempts)
	}
	if finalStatus.Subtasks[0].Result != "recovered output" {
		t.Errorf("unexpected subtask result: %q", finalStatus.Subtasks[0].Result)
	}
}

func TestAddMissionRPC(t *testing.T) {
	token := "token-add-mission"
	q := NewQueue("", 10*time.Second, 60*time.Second)
	srv := NewServer("127.0.0.1:0", token, q)
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(context.Background())

	leaderURL := "http://" + srv.Addr() + "/rpc"

	call := func(payload string) (int, *RPCResponse, error) {
		req, _ := http.NewRequest(http.MethodPost, leaderURL, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderMeshToken, token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		var rpcResp RPCResponse
		err = json.NewDecoder(resp.Body).Decode(&rpcResp)
		return resp.StatusCode, &rpcResp, err
	}

	// 1. Invalid params: 0 subtasks -> error -32602
	_, r1, err := call(`{"jsonrpc":"2.0","id":1,"method":"mesh.add_mission","params":{"mission":"test","subtasks":[]}}`)
	if err != nil || r1.Error == nil || r1.Error.Code != -32602 {
		t.Errorf("expected error -32602 for 0 subtasks, got: %+v (err: %v)", r1, err)
	}

	// 2. Invalid params: empty goal -> error -32602
	_, r2, err := call(`{"jsonrpc":"2.0","id":2,"method":"mesh.add_mission","params":{"mission":"test","subtasks":[{"goal":"  "}]}}`)
	if err != nil || r2.Error == nil || r2.Error.Code != -32602 {
		t.Errorf("expected error -32602 for empty goal, got: %+v (err: %v)", r2, err)
	}

	// 3. Invalid params: empty mission -> error -32602
	_, r3, err := call(`{"jsonrpc":"2.0","id":3,"method":"mesh.add_mission","params":{"mission":"   ","subtasks":[{"goal":"valid"}]}}`)
	if err != nil || r3.Error == nil || r3.Error.Code != -32602 {
		t.Errorf("expected error -32602 for empty mission, got: %+v (err: %v)", r3, err)
	}

	// 4. Happy path -> success, result mission state
	_, r4, err := call(`{"jsonrpc":"2.0","id":4,"method":"mesh.add_mission","params":{"mission":"valid mission","subtasks":[{"goal":"step 1"},{"goal":"step 2"}]}}`)
	if err != nil || r4.Error != nil {
		t.Fatalf("expected success, got error: %+v (err: %v)", r4, err)
	}
	resBytes, _ := json.Marshal(r4.Result)
	var m MissionState
	if err := json.Unmarshal(resBytes, &m); err != nil {
		t.Fatalf("failed to unmarshal mission result: %v", err)
	}
	if m.Text != "valid mission" || len(m.Subtasks) != 2 {
		t.Errorf("unexpected mission state: %+v", m)
	}
}

func TestAutoFanoutOnBudgetExhaustion(t *testing.T) {
	var runCount int
	var planCount int
	var mu sync.Mutex

	client := NewClient("http://127.0.0.1:9999", "tok", "w-fanout", []string{"general"}, func(ctx context.Context, goal string) (string, error) {
		mu.Lock()
		runCount++
		c := runCount
		mu.Unlock()

		if c == 1 {
			return "", fmt.Errorf("agent hit the 8-iteration tool budget without a final answer: max iterations exceeded")
		}
		// Sub-run
		return "SUB-" + goal, nil
	})
	client.SetFanoutMax(3)
	client.SetPlanFunc(func(ctx context.Context, goal string) (string, error) {
		mu.Lock()
		planCount++
		mu.Unlock()
		return `["part a", "part b", "part c"]`, nil
	})

	task := &Task{
		ID:         "task-fanout-1",
		MissionID:  "m-fanout",
		Goal:       "Complex research",
		TimeoutSec: 30,
	}

	result, iters, err := client.runTaskWithFanout(context.Background(), task, true)
	if err != nil {
		t.Fatalf("expected successful fanout execution, got err: %v", err)
	}

	mu.Lock()
	pCount := planCount
	rCount := runCount
	mu.Unlock()

	if pCount != 1 {
		t.Errorf("expected planFunc called 1 time, got %d", pCount)
	}
	// Initial run (1) + 3 sub-runs = 4 total runFunc calls
	if rCount != 4 {
		t.Errorf("expected 4 total runFunc calls (1 initial + 3 sub-runs), got %d", rCount)
	}
	if iters != 3 {
		t.Errorf("expected iterations 3, got %d", iters)
	}
	if !strings.Contains(result, "## Sub-result 1\nSUB-part a") {
		t.Errorf("missing sub-result 1 in %q", result)
	}
	if !strings.Contains(result, "## Sub-result 2\nSUB-part b") {
		t.Errorf("missing sub-result 2 in %q", result)
	}
	if !strings.Contains(result, "## Sub-result 3\nSUB-part c") {
		t.Errorf("missing sub-result 3 in %q", result)
	}
}

func TestFanoutDisabledOrNoBudget(t *testing.T) {
	var planCalled bool
	client := NewClient("http://127.0.0.1:9999", "tok", "w-nofanout", []string{"general"}, func(ctx context.Context, goal string) (string, error) {
		return "", fmt.Errorf("max iterations exceeded")
	})
	client.SetFanoutMax(0) // Disabled
	client.SetPlanFunc(func(ctx context.Context, goal string) (string, error) {
		planCalled = true
		return `["a", "b"]`, nil
	})

	task := &Task{
		ID:         "task-nofanout",
		MissionID:  "m-nofanout",
		Goal:       "Do work",
		TimeoutSec: 10,
	}

	_, _, err := client.runTaskWithFanout(context.Background(), task, true)
	if err == nil || !strings.Contains(err.Error(), "max iterations exceeded") {
		t.Errorf("expected original error reported, got: %v", err)
	}
	if planCalled {
		t.Errorf("planFunc should not be called when FANOUT_MAX is 0")
	}
}

func TestFanoutRecursionGuard(t *testing.T) {
	var planCount int
	var mu sync.Mutex

	client := NewClient("http://127.0.0.1:9999", "tok", "w-guard", []string{"general"}, func(ctx context.Context, goal string) (string, error) {
		// All calls, including sub-runs, return budget exhaustion error
		return "", fmt.Errorf("agent hit the 8-iteration tool budget without a final answer")
	})
	client.SetFanoutMax(3)
	client.SetPlanFunc(func(ctx context.Context, goal string) (string, error) {
		mu.Lock()
		planCount++
		mu.Unlock()
		return `["sub 1", "sub 2"]`, nil
	})

	task := &Task{
		ID:         "task-guard",
		MissionID:  "m-guard",
		Goal:       "Recurse test",
		TimeoutSec: 10,
	}

	_, _, err := client.runTaskWithFanout(context.Background(), task, true)
	if err == nil {
		t.Fatal("expected error when all sub-goals fail")
	}

	mu.Lock()
	count := planCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected planFunc called exactly 1 time (no recursion), got %d", count)
	}
}

// Gap-1: a re-registering worker whose in-flight task was requeued by leader
// restart replay gets it back in LostTasks and aborts locally.
func TestRegisterClaimCheckLostTasks(t *testing.T) {
	q := NewQueue(t.TempDir(), time.Minute, 5*time.Minute)
	q.RegisterWorker("w1", []string{"general"})
	st, err := q.AddMission("m", []SubtaskInput{{Goal: "g1"}})
	if err != nil {
		t.Fatal(err)
	}
	// w1 pulls the task (in-flight), then the leader "restarts": replay
	// requeues in-flight work.
	task, err := q.GetNextTask("w1", 0)
	if err != nil || task == nil {
		t.Fatalf("no task: %v", err)
	}
	// simulate what replay() does at startup: in-flight -> pending
	q.mu.Lock()
	delete(q.inFlightTasks, task.ID)
	delete(q.workerInFlight["w1"], task.ID)
	task.AssignedTo = ""
	q.pendingTasks = append(q.pendingTasks, task)
	q.mu.Unlock()

	lost := q.LostTasks("w1", []string{task.ID})
	if len(lost) != 1 || lost[0] != task.ID {
		t.Fatalf("expected task %s lost, got %v", task.ID, lost)
	}
	_ = st
}

// Gap-2: mission final result persists and replays across "restart".
func TestMissionResultPersistReplay(t *testing.T) {
	dir := t.TempDir()
	q := NewQueue(dir, time.Minute, 5*time.Minute)
	st, err := q.AddMission("m", []SubtaskInput{{Goal: "g1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.SetMissionResult(st.ID, "FINAL ANSWER 42"); err != nil {
		t.Fatal(err)
	}
	// fresh queue over same dataDir = leader restart replay
	q2 := NewQueue(dir, time.Minute, 5*time.Minute)
	st2, err := q2.MissionStatus(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st2.FinalResult != "FINAL ANSWER 42" {
		t.Fatalf("final result lost on replay: %q", st2.FinalResult)
	}
}
