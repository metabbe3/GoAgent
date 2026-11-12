package mesh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	client := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/nexttask?id=w1", nil)

	// No token -> 401
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized without token, got %d", resp.StatusCode)
	}

	// Wrong token -> 401
	req.Header.Set(HeaderMeshToken, "wrong-token")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized with wrong token, got %d", resp.StatusCode)
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
