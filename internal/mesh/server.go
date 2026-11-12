package mesh

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// Server coordinates the GoMesh cluster as a leader.
type Server struct {
	listenAddr string
	token      string
	queue      *Queue
	server     *http.Server
	listener   net.Listener
	mu         sync.Mutex
	stopChan   chan struct{}
}

// NewServer initializes a GoMesh HTTP/1.1 leader server.
func NewServer(listenAddr, token string, queue *Queue) *Server {
	return &Server{
		listenAddr: listenAddr,
		token:      token,
		queue:      queue,
		stopChan:   make(chan struct{}),
	}
}

// Queue returns the server's task queue.
func (s *Server) Queue() *Queue {
	return s.queue
}

// Start begins listening on the configured address and runs the liveness sweeper.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.listenAddr, err)
	}
	s.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", s.handleRPC)
	mux.HandleFunc("/nexttask", s.handleNextTask)

	handler := s.authMiddleware(mux)
	s.server = &http.Server{
		Handler: handler,
	}

	// Start liveness sweeper: runs every 15s to check 90s heartbeat timeouts & mission timeouts
	go s.sweeperLoop()

	log.Printf("[mesh] leader listening on %s (proto: %s)", ln.Addr().String(), MeshProtocol)
	go func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[mesh] leader server error: %v", err)
		}
	}()

	return nil
}

// Addr returns the bound address of the server listener (useful in tests).
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.listenAddr
}

// Stop gracefully shuts down the leader server and liveness sweeper.
func (s *Server) Stop(ctx context.Context) error {
	close(s.stopChan)
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(HeaderMeshToken)
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			log.Printf("[mesh] unauthorized request to %s from %s", r.URL.Path, r.RemoteAddr)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req RPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(HeaderMeshProto, ProtoVersion)

	switch req.Method {
	case "mesh.register":
		var p RegisterReq
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Worker.ID == "" {
			writeRPCError(w, req.ID, -32602, "invalid params for mesh.register")
			return
		}
		wInfo := s.queue.RegisterWorker(p.Worker.ID, p.Worker.Capabilities)
		resp := RegisterResp{
			WorkerID:     wInfo.ID,
			HeartbeatSec: HeartbeatSec,
			Poll:         "/nexttask",
		}
		writeRPCSuccess(w, req.ID, resp)

	case "mesh.heartbeat":
		var p HeartbeatReq
		if err := json.Unmarshal(req.Params, &p); err != nil || p.WorkerID == "" {
			writeRPCError(w, req.ID, -32602, "invalid params for mesh.heartbeat")
			return
		}
		pending, err := s.queue.Heartbeat(p.WorkerID)
		if err != nil {
			writeRPCError(w, req.ID, -32000, err.Error())
			return
		}
		resp := HeartbeatResp{
			OK:           true,
			TasksPending: pending,
		}
		writeRPCSuccess(w, req.ID, resp)

	case "mesh.report":
		var p ReportReq
		if err := json.Unmarshal(req.Params, &p); err != nil || p.WorkerID == "" || p.TaskID == "" {
			writeRPCError(w, req.ID, -32602, "invalid params for mesh.report")
			return
		}
		if err := s.queue.ReportTask(p); err != nil {
			writeRPCError(w, req.ID, -32000, err.Error())
			return
		}
		writeRPCSuccess(w, req.ID, ReportResp{OK: true})

	default:
		writeRPCError(w, req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) handleNextTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	workerID := r.URL.Query().Get("id")
	if workerID == "" {
		http.Error(w, "missing id parameter", http.StatusBadRequest)
		return
	}

	w.Header().Set(HeaderMeshProto, ProtoVersion)

	// Long poll up to 50 seconds
	task, err := s.queue.GetNextTask(workerID, DefaultPollSec*time.Second)
	if err != nil || task == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(task)
}

func (s *Server) sweeperLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopChan:
			return
		case <-ticker.C:
			s.queue.SweepDeadWorkers(DeadTimeoutSec * time.Second)
			s.queue.SweepMissionTimeouts()
		}
	}
}

func writeRPCSuccess(w http.ResponseWriter, id any, result any) {
	resp := RPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func writeRPCError(w http.ResponseWriter, id any, code int, message string) {
	resp := RPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &RPCError{
			Code:    code,
			Message: message,
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}
