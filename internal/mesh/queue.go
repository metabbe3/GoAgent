package mesh

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// SubtaskInput represents a requested subtask for a mission.
type SubtaskInput struct {
	Goal       string `json:"goal"`
	Context    string `json:"context,omitempty"`
	Capability string `json:"capability,omitempty"`
}

type QueueEvent struct {
	Type      string        `json:"type"`
	Time      time.Time     `json:"time"`
	Mission   *MissionState `json:"mission,omitempty"`
	TaskID    string        `json:"task_id,omitempty"`
	MissionID string        `json:"mission_id,omitempty"`
	WorkerID  string        `json:"worker_id,omitempty"`
	Report    *ReportReq    `json:"report,omitempty"`
}

type waiter struct {
	workerID string
	caps     []string
	ch       chan *Task
}

// Queue coordinates missions, subtasks, worker routing, and liveness.
type Queue struct {
	mu             sync.Mutex
	dataDir        string
	stateFile      string
	workers        map[string]*WorkerInfo
	missions       map[string]*MissionState
	tasks          map[string]*Task
	pendingTasks   []*Task
	inFlightTasks  map[string]*Task
	workerInFlight map[string]map[string]bool
	waiters        map[string]*waiter
	rrIndex        map[string]int
	taskTimeout    time.Duration
	missionTimeout time.Duration
}

// NewQueue creates and initializes a Queue with optional state replay.
func NewQueue(dataDir string, taskTimeout, missionTimeout time.Duration) *Queue {
	if taskTimeout <= 0 {
		taskTimeout = 300 * time.Second
	}
	if missionTimeout <= 0 {
		missionTimeout = 1800 * time.Second
	}

	stateFile := ""
	if dataDir != "" {
		meshDir := filepath.Join(dataDir, "mesh")
		_ = os.MkdirAll(meshDir, 0o700)
		stateFile = filepath.Join(meshDir, "state.jsonl")
	}

	q := &Queue{
		dataDir:        dataDir,
		stateFile:      stateFile,
		workers:        make(map[string]*WorkerInfo),
		missions:       make(map[string]*MissionState),
		tasks:          make(map[string]*Task),
		pendingTasks:   make([]*Task, 0),
		inFlightTasks:  make(map[string]*Task),
		workerInFlight: make(map[string]map[string]bool),
		waiters:        make(map[string]*waiter),
		rrIndex:        make(map[string]int),
		taskTimeout:    taskTimeout,
		missionTimeout: missionTimeout,
	}

	if stateFile != "" {
		q.replay()
	}
	return q
}

// RegisterWorker records or updates a worker in the cluster registry.
func (q *Queue) RegisterWorker(id string, caps []string) *WorkerInfo {
	q.mu.Lock()
	defer q.mu.Unlock()

	w, ok := q.workers[id]
	if !ok {
		w = &WorkerInfo{
			ID:           id,
			Capabilities: caps,
			LastSeen:     time.Now(),
			Status:       "active",
		}
		q.workers[id] = w
		q.workerInFlight[id] = make(map[string]bool)
	} else {
		w.Capabilities = caps
		w.LastSeen = time.Now()
		w.Status = "active"
	}
	log.Printf("[mesh] worker registered: %s (caps: %v)", id, caps)
	return w
}

// Heartbeat refreshes worker liveness and returns count of pending tasks.
func (q *Queue) Heartbeat(workerID string) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	w, ok := q.workers[workerID]
	if !ok {
		w = &WorkerInfo{
			ID:       workerID,
			LastSeen: time.Now(),
			Status:   "active",
		}
		q.workers[workerID] = w
		q.workerInFlight[workerID] = make(map[string]bool)
	} else {
		w.LastSeen = time.Now()
		w.Status = "active"
	}
	return len(q.pendingTasks), nil
}

// ListWorkers returns snapshots of all registered workers.
func (q *Queue) ListWorkers() []WorkerInfo {
	q.mu.Lock()
	defer q.mu.Unlock()

	out := make([]WorkerInfo, 0, len(q.workers))
	for _, w := range q.workers {
		out = append(out, *w)
	}
	return out
}

// AddMission enqueues a new mission with its subtasks.
func (q *Queue) AddMission(text string, subtasks []SubtaskInput) (*MissionState, error) {
	if len(subtasks) == 0 {
		return nil, fmt.Errorf("mission must have at least one subtask")
	}

	missionID := generateID("m")
	now := time.Now()

	mission := &MissionState{
		ID:        missionID,
		Text:      text,
		Status:    "pending",
		CreatedAt: now,
		Subtasks:  make([]SubtaskState, len(subtasks)),
	}

	newTasks := make([]*Task, len(subtasks))
	for i, st := range subtasks {
		taskID := fmt.Sprintf("%s-t%d", missionID, i+1)
		subtask := SubtaskState{
			ID:         taskID,
			Goal:       st.Goal,
			Context:    st.Context,
			Capability: st.Capability,
			Status:     "pending",
			CreatedAt:  now,
		}
		mission.Subtasks[i] = subtask

		task := &Task{
			ID:         taskID,
			MissionID:  missionID,
			Goal:       st.Goal,
			Context:    st.Context,
			Capability: st.Capability,
			TimeoutSec: int(q.taskTimeout.Seconds()),
			CreatedAt:  now,
		}
		newTasks[i] = task
	}

	q.mu.Lock()
	q.missions[missionID] = mission
	for _, t := range newTasks {
		q.tasks[t.ID] = t
		q.pendingTasks = append(q.pendingTasks, t)
	}
	q.appendEvent(QueueEvent{
		Type:    "mission_added",
		Time:    now,
		Mission: mission,
	})
	log.Printf("[mesh] mission %s added with %d subtasks", missionID, len(subtasks))

	// Try dispatching immediately to matching long-polling workers
	q.dispatchPendingLocked()
	q.mu.Unlock()

	return mission, nil
}

// GetNextTask retrieves the next matching task for a worker, or blocks until maxWait.
func (q *Queue) GetNextTask(workerID string, maxWait time.Duration) (*Task, error) {
	q.mu.Lock()
	w, ok := q.workers[workerID]
	if !ok {
		w = &WorkerInfo{
			ID:       workerID,
			LastSeen: time.Now(),
			Status:   "active",
		}
		q.workers[workerID] = w
		q.workerInFlight[workerID] = make(map[string]bool)
	} else {
		w.LastSeen = time.Now()
	}

	// 1. Check if a pending task matches this worker immediately
	if task := q.claimPendingTaskForWorkerLocked(w); task != nil {
		q.mu.Unlock()
		return task, nil
	}

	// 2. No task ready: register waiter for long-poll
	waitCh := make(chan *Task, 1)
	q.waiters[workerID] = &waiter{
		workerID: workerID,
		caps:     w.Capabilities,
		ch:       waitCh,
	}
	q.mu.Unlock()

	// Wait on channel or timeout
	select {
	case task := <-waitCh:
		return task, nil
	case <-time.After(maxWait):
		q.mu.Lock()
		select {
		case task := <-waitCh:
			q.mu.Unlock()
			return task, nil
		default:
		}
		delete(q.waiters, workerID)
		q.mu.Unlock()
		return nil, nil
	}
}

func (q *Queue) claimPendingTaskForWorkerLocked(w *WorkerInfo) *Task {
	for i, task := range q.pendingTasks {
		if workerMatchesCapability(w.Capabilities, task.Capability) {
			// Found matching task
			q.pendingTasks = append(q.pendingTasks[:i], q.pendingTasks[i+1:]...)
			q.assignTaskLocked(task, w.ID)
			return task
		}
	}
	return nil
}

func (q *Queue) assignTaskLocked(task *Task, workerID string) {
	task.AssignedTo = workerID
	q.inFlightTasks[task.ID] = task

	if q.workerInFlight[workerID] == nil {
		q.workerInFlight[workerID] = make(map[string]bool)
	}
	q.workerInFlight[workerID][task.ID] = true

	if w, ok := q.workers[workerID]; ok {
		w.InFlight = len(q.workerInFlight[workerID])
	}

	if m, ok := q.missions[task.MissionID]; ok {
		if m.Status == "pending" {
			m.Status = "in_progress"
		}
		for i := range m.Subtasks {
			if m.Subtasks[i].ID == task.ID {
				m.Subtasks[i].Status = "in_flight"
				m.Subtasks[i].AssignedTo = workerID
				break
			}
		}
	}

	q.appendEvent(QueueEvent{
		Type:      "task_assigned",
		Time:      time.Now(),
		TaskID:    task.ID,
		MissionID: task.MissionID,
		WorkerID:  workerID,
	})
	log.Printf("[mesh] task %s assigned to worker %s (mission: %s)", task.ID, workerID, task.MissionID)
}

func (q *Queue) dispatchPendingLocked() {
	if len(q.pendingTasks) == 0 || len(q.waiters) == 0 {
		return
	}

	remPending := q.pendingTasks[:0]
	for _, task := range q.pendingTasks {
		// Find matching waiters
		var matchingWaiters []*waiter
		for _, wt := range q.waiters {
			if workerMatchesCapability(wt.caps, task.Capability) {
				matchingWaiters = append(matchingWaiters, wt)
			}
		}

		if len(matchingWaiters) == 0 {
			remPending = append(remPending, task)
			continue
		}

		// Round-robin selection among matching waiters
		capKey := task.Capability
		idx := q.rrIndex[capKey] % len(matchingWaiters)
		q.rrIndex[capKey]++
		chosen := matchingWaiters[idx]

		delete(q.waiters, chosen.workerID)
		q.assignTaskLocked(task, chosen.workerID)
		chosen.ch <- task
	}
	q.pendingTasks = remPending
}

// ReportTask records completion or failure of an in-flight task.
func (q *Queue) ReportTask(req ReportReq) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	_, ok := q.inFlightTasks[req.TaskID]
	if !ok {
		// Task might have already timed out or completed
		log.Printf("[mesh] ignoring report for untracked task %s from worker %s", req.TaskID, req.WorkerID)
		return nil
	}

	delete(q.inFlightTasks, req.TaskID)
	if inflight, ok := q.workerInFlight[req.WorkerID]; ok {
		delete(inflight, req.TaskID)
	}
	if w, ok := q.workers[req.WorkerID]; ok {
		w.InFlight = len(q.workerInFlight[req.WorkerID])
	}

	now := time.Now()
	if m, ok := q.missions[req.MissionID]; ok {
		for i := range m.Subtasks {
			if m.Subtasks[i].ID == req.TaskID {
				if req.Success {
					m.Subtasks[i].Status = "completed"
				} else {
					m.Subtasks[i].Status = "failed"
				}
				m.Subtasks[i].Result = req.Result
				m.Subtasks[i].Error = req.Error
				m.Subtasks[i].Metrics = req.Metrics
				m.Subtasks[i].CompletedAt = now
				break
			}
		}

		// Check if all subtasks in this mission finished
		allDone := true
		anyFailed := false
		for _, st := range m.Subtasks {
			if st.Status == "pending" || st.Status == "in_flight" {
				allDone = false
				break
			}
			if st.Status == "failed" {
				anyFailed = true
			}
		}
		if allDone {
			if anyFailed {
				m.Status = "failed"
			} else {
				m.Status = "completed"
			}
			m.CompletedAt = now
			log.Printf("[mesh] mission %s %s", m.ID, m.Status)
		}
	}

	q.appendEvent(QueueEvent{
		Type:      "task_reported",
		Time:      now,
		TaskID:    req.TaskID,
		MissionID: req.MissionID,
		WorkerID:  req.WorkerID,
		Report:    &req,
	})
	log.Printf("[mesh] task %s reported by %s: success=%v", req.TaskID, req.WorkerID, req.Success)

	// Dispatch remaining pending tasks if waiters are ready
	q.dispatchPendingLocked()
	return nil
}

// CancelMission cancels an active or pending mission.
func (q *Queue) CancelMission(missionID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	m, ok := q.missions[missionID]
	if !ok {
		return fmt.Errorf("mission %s not found", missionID)
	}
	if m.Status == "completed" || m.Status == "cancelled" {
		return nil
	}

	now := time.Now()
	m.Status = "cancelled"
	m.CompletedAt = now

	// Remove pending tasks for this mission
	kept := q.pendingTasks[:0]
	for _, t := range q.pendingTasks {
		if t.MissionID != missionID {
			kept = append(kept, t)
		}
	}
	q.pendingTasks = kept

	// Mark subtasks cancelled
	for i := range m.Subtasks {
		if m.Subtasks[i].Status == "pending" || m.Subtasks[i].Status == "in_flight" {
			m.Subtasks[i].Status = "cancelled"
			m.Subtasks[i].CompletedAt = now
		}
	}

	q.appendEvent(QueueEvent{
		Type:      "mission_cancelled",
		Time:      now,
		MissionID: missionID,
	})
	log.Printf("[mesh] mission %s cancelled", missionID)
	return nil
}

// MissionStatus returns the full state of a mission.
func (q *Queue) MissionStatus(missionID string) (*MissionState, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	m, ok := q.missions[missionID]
	if !ok {
		return nil, fmt.Errorf("mission %s not found", missionID)
	}
	mCopy := *m
	mCopy.Subtasks = append([]SubtaskState{}, m.Subtasks...)
	return &mCopy, nil
}

// CollectResults combines completed subtask results for a mission.
func (q *Queue) CollectResults(missionID string) (string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	m, ok := q.missions[missionID]
	if !ok {
		return "", fmt.Errorf("mission %s not found", missionID)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Mission %s: %s (Status: %s)\n\n", m.ID, m.Text, m.Status)
	for i, st := range m.Subtasks {
		fmt.Fprintf(&sb, "### Subtask %d: %s\n", i+1, st.Goal)
		fmt.Fprintf(&sb, "Status: %s | Worker: %s\n", st.Status, st.AssignedTo)
		if st.Result != "" {
			fmt.Fprintf(&sb, "Result:\n%s\n", st.Result)
		}
		if st.Error != "" {
			fmt.Fprintf(&sb, "Error:\n%s\n", st.Error)
		}
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String()), nil
}

// SweepDeadWorkers detects workers with 3 missed heartbeats (>deadTimeout) and requeues in-flight tasks.
func (q *Queue) SweepDeadWorkers(deadTimeout time.Duration) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	requeuedCount := 0

	for id, w := range q.workers {
		if now.Sub(w.LastSeen) > deadTimeout {
			if w.Status != "dead" {
				w.Status = "dead"
				log.Printf("[mesh] worker %s marked dead (last seen %v ago)", id, now.Sub(w.LastSeen))
			}

			// Requeue in-flight tasks for this dead worker
			if inflight, ok := q.workerInFlight[id]; ok && len(inflight) > 0 {
				for taskID := range inflight {
					task, exists := q.inFlightTasks[taskID]
					if !exists {
						continue
					}
					delete(q.inFlightTasks, taskID)
					delete(inflight, taskID)

					task.AssignedTo = ""
					q.pendingTasks = append(q.pendingTasks, task)

					if m, ok := q.missions[task.MissionID]; ok {
						for i := range m.Subtasks {
							if m.Subtasks[i].ID == task.ID {
								m.Subtasks[i].Status = "pending"
								m.Subtasks[i].AssignedTo = ""
								break
							}
						}
					}
					requeuedCount++
					log.Printf("[mesh] requeued in-flight task %s from dead worker %s", taskID, id)
				}
				w.InFlight = 0
			}
		}
	}

	if requeuedCount > 0 {
		q.dispatchPendingLocked()
	}
	return requeuedCount
}

// SweepMissionTimeouts cancels missions older than missionTimeout.
func (q *Queue) SweepMissionTimeouts() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	now := time.Now()
	cancelled := 0

	for id, m := range q.missions {
		if (m.Status == "pending" || m.Status == "in_progress") && now.Sub(m.CreatedAt) > q.missionTimeout {
			m.Status = "cancelled"
			m.CompletedAt = now
			cancelled++

			// Remove pending tasks
			kept := q.pendingTasks[:0]
			for _, t := range q.pendingTasks {
				if t.MissionID != id {
					kept = append(kept, t)
				}
			}
			q.pendingTasks = kept

			for i := range m.Subtasks {
				if m.Subtasks[i].Status == "pending" || m.Subtasks[i].Status == "in_flight" {
					m.Subtasks[i].Status = "cancelled"
					m.Subtasks[i].CompletedAt = now
				}
			}
			log.Printf("[mesh] mission %s timed out after %v, cancelled", id, q.missionTimeout)
		}
	}
	return cancelled
}

func (q *Queue) appendEvent(ev QueueEvent) {
	if q.stateFile == "" {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f, err := os.OpenFile(q.stateFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

func (q *Queue) replay() {
	data, err := os.ReadFile(q.stateFile)
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev QueueEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "mission_added":
			if ev.Mission != nil {
				q.missions[ev.Mission.ID] = ev.Mission
				for _, st := range ev.Mission.Subtasks {
					t := &Task{
						ID:         st.ID,
						MissionID:  ev.Mission.ID,
						Goal:       st.Goal,
						Context:    st.Context,
						Capability: st.Capability,
						CreatedAt:  ev.Mission.CreatedAt,
					}
					q.tasks[t.ID] = t
					if st.Status == "pending" {
						q.pendingTasks = append(q.pendingTasks, t)
					}
				}
			}
		case "task_assigned":
			if t, ok := q.tasks[ev.TaskID]; ok {
				t.AssignedTo = ev.WorkerID
				q.inFlightTasks[t.ID] = t
				// Remove from pending
				for i, pt := range q.pendingTasks {
					if pt.ID == ev.TaskID {
						q.pendingTasks = append(q.pendingTasks[:i], q.pendingTasks[i+1:]...)
						break
					}
				}
				if m, ok := q.missions[ev.MissionID]; ok {
					for i := range m.Subtasks {
						if m.Subtasks[i].ID == ev.TaskID {
							m.Subtasks[i].Status = "in_flight"
							m.Subtasks[i].AssignedTo = ev.WorkerID
							break
						}
					}
				}
			}
		case "task_reported":
			if ev.Report != nil {
				delete(q.inFlightTasks, ev.Report.TaskID)
				if m, ok := q.missions[ev.Report.MissionID]; ok {
					for i := range m.Subtasks {
						if m.Subtasks[i].ID == ev.Report.TaskID {
							if ev.Report.Success {
								m.Subtasks[i].Status = "completed"
							} else {
								m.Subtasks[i].Status = "failed"
							}
							m.Subtasks[i].Result = ev.Report.Result
							m.Subtasks[i].Error = ev.Report.Error
							m.Subtasks[i].Metrics = ev.Report.Metrics
							m.Subtasks[i].CompletedAt = ev.Time
							break
						}
					}
				}
			}
		case "mission_cancelled":
			if m, ok := q.missions[ev.MissionID]; ok {
				m.Status = "cancelled"
				m.CompletedAt = ev.Time
				// Remove any remaining pending tasks
				kept := q.pendingTasks[:0]
				for _, t := range q.pendingTasks {
					if t.MissionID != ev.MissionID {
						kept = append(kept, t)
					}
				}
				q.pendingTasks = kept
			}
		}
	}

	// Any tasks that were left in-flight when server crashed are restored to pending
	for _, t := range q.inFlightTasks {
		t.AssignedTo = ""
		q.pendingTasks = append(q.pendingTasks, t)
		if m, ok := q.missions[t.MissionID]; ok {
			for i := range m.Subtasks {
				if m.Subtasks[i].ID == t.ID {
					m.Subtasks[i].Status = "pending"
					m.Subtasks[i].AssignedTo = ""
					break
				}
			}
		}
	}
	q.inFlightTasks = make(map[string]*Task)
	log.Printf("[mesh] replay restored %d missions, %d pending tasks", len(q.missions), len(q.pendingTasks))
}

func workerMatchesCapability(workerCaps []string, taskCap string) bool {
	if taskCap == "" {
		return true // General tasks match any worker
	}
	for _, c := range workerCaps {
		if strings.EqualFold(strings.TrimSpace(c), strings.TrimSpace(taskCap)) {
			return true
		}
	}
	return false
}

func generateID(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s", prefix, time.Now().Format("150405"), hex.EncodeToString(b))
}
