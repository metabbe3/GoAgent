package mesh

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/metabbe3/GoAgent/internal/guardrail"
)

// RunFunc is the callback that executes a task locally.
// It decouples the mesh worker from the concrete agent implementation.
type RunFunc func(ctx context.Context, goal string) (string, error)

// Client runs a worker node connected to a GoMesh leader.
type Client struct {
	leaderAddr   string
	token        string
	workerID     string
	capabilities []string
	runFunc      RunFunc
	httpClient   *http.Client
	backoffBase  time.Duration
	stopChan     chan struct{}
}

// NewClient initializes a worker client.
func NewClient(leaderAddr, token, workerID string, capabilities []string, runFunc RunFunc) *Client {
	return &Client{
		leaderAddr:   strings.TrimRight(leaderAddr, "/"),
		token:        token,
		workerID:     workerID,
		capabilities: capabilities,
		runFunc:      runFunc,
		httpClient: &http.Client{
			Timeout: 65 * time.Second, // Long-poll is 50s; allow buffer for network
		},
		stopChan: make(chan struct{}),
	}
}

// Start runs the registration, heartbeat, and long-poll loop until ctx is cancelled.
func (c *Client) Start(ctx context.Context) error {
	log.Printf("[mesh] worker %s starting, connecting to leader at %s", c.workerID, c.leaderAddr)

	// 1. Register with leader (with backoff retry)
	if err := c.registerWithRetry(ctx); err != nil {
		return err
	}

	// 2. Start heartbeat goroutine
	go c.heartbeatLoop(ctx)

	// 3. Start task polling loop
	return c.pollLoop(ctx)
}

// Stop terminates the worker loops.
func (c *Client) Stop() {
	close(c.stopChan)
}

func (c *Client) registerWithRetry(ctx context.Context) error {
	base := c.backoffBase
	if base <= 0 {
		base = 1 * time.Second
	}
	backoff := base
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopChan:
			return nil
		default:
		}

		err := c.registerOnce(ctx)
		if err == nil {
			log.Printf("[mesh] worker %s successfully registered with leader", c.workerID)
			return nil
		}

		log.Printf("[mesh] registration failed: %v, retrying in %v...", err, backoff)
		select {
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopChan:
			return nil
		}
	}
}

func (c *Client) registerOnce(ctx context.Context) error {
	params := RegisterReq{}
	params.Worker.ID = c.workerID
	params.Worker.Capabilities = c.capabilities

	rawParams, err := json.Marshal(params)
	if err != nil {
		return err
	}

	reqBody := RPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "mesh.register",
		Params:  rawParams,
	}

	var resp RegisterResp
	return c.callRPC(ctx, reqBody, &resp)
}

func (c *Client) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(HeartbeatSec * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopChan:
			return
		case <-ticker.C:
			params := HeartbeatReq{WorkerID: c.workerID}
			rawParams, _ := json.Marshal(params)
			reqBody := RPCRequest{
				JSONRPC: "2.0",
				ID:      time.Now().Unix(),
				Method:  "mesh.heartbeat",
				Params:  rawParams,
			}
			var resp HeartbeatResp
			if err := c.callRPC(ctx, reqBody, &resp); err != nil {
				log.Printf("[mesh] heartbeat error: %v", err)
			}
		}
	}
}

func (c *Client) pollLoop(ctx context.Context) error {
	backoff := 1 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stopChan:
			return nil
		default:
		}

		task, err := c.fetchNextTask(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			select {
			case <-c.stopChan:
				return nil
			default:
			}
			log.Printf("[mesh] error polling next task: %v (backoff %v)", err, backoff)
			select {
			case <-time.After(backoff):
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			case <-ctx.Done():
				return ctx.Err()
			case <-c.stopChan:
				return nil
			}
			continue
		}

		// Reset backoff on successful communication
		backoff = 1 * time.Second

		if task == nil {
			// 204 No Content timeout: continue polling immediately
			continue
		}

		// Execute task
		log.Printf("[mesh] worker %s executing task %s (goal: %q)", c.workerID, task.ID, task.Goal)
		start := time.Now()

		var result string
		var execErr error
		if c.runFunc != nil {
			result, execErr = c.runFunc(ctx, task.Goal)
		} else {
			execErr = fmt.Errorf("no RunFunc configured on worker")
		}
		elapsed := time.Since(start)

		// Guardrail: redact output before reporting to leader
		redactedResult, _ := guardrail.Redact(result)

		errMsg := ""
		if execErr != nil {
			errMsg = execErr.Error()
		}

		// Report completion
		report := ReportReq{
			WorkerID:  c.workerID,
			TaskID:    task.ID,
			MissionID: task.MissionID,
			Success:   execErr == nil,
			Result:    redactedResult,
			Error:     errMsg,
			Metrics: ReportMetrics{
				Iterations: 1,
				ElapsedMs:  elapsed.Milliseconds(),
			},
		}

		if err := c.reportTask(ctx, report); err != nil {
			log.Printf("[mesh] failed to report task %s: %v", task.ID, err)
		} else {
			log.Printf("[mesh] reported task %s successfully (elapsed: %v)", task.ID, elapsed)
		}
	}
}

func (c *Client) fetchNextTask(ctx context.Context) (*Task, error) {
	url := fmt.Sprintf("%s/nexttask?id=%s", c.leaderAddr, c.workerID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set(HeaderMeshToken, c.token)
	req.Header.Set(HeaderMeshProto, ProtoVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("leader authentication failed: 401 Unauthorized")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status from leader: HTTP %d", resp.StatusCode)
	}

	var task Task
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return nil, fmt.Errorf("failed to decode task: %w", err)
	}
	return &task, nil
}

func (c *Client) reportTask(ctx context.Context, report ReportReq) error {
	rawParams, err := json.Marshal(report)
	if err != nil {
		return err
	}

	reqBody := RPCRequest{
		JSONRPC: "2.0",
		ID:      time.Now().Unix(),
		Method:  "mesh.report",
		Params:  rawParams,
	}

	var resp ReportResp
	return c.callRPC(ctx, reqBody, &resp)
}

func (c *Client) callRPC(ctx context.Context, req RPCRequest, resultTarget any) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}

	url := c.leaderAddr + "/rpc"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(HeaderMeshToken, c.token)
	httpReq.Header.Set(HeaderMeshProto, ProtoVersion)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("leader authentication failed: 401 Unauthorized")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rpc HTTP error %d: %s", resp.StatusCode, string(body))
	}

	var rpcResp RPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return fmt.Errorf("failed to decode rpc response: %w", err)
	}

	if rpcResp.Error != nil {
		return fmt.Errorf("rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	if resultTarget != nil && rpcResp.Result != nil {
		rawRes, err := json.Marshal(rpcResp.Result)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(rawRes, resultTarget); err != nil {
			return fmt.Errorf("failed to parse rpc result: %w", err)
		}
	}
	return nil
}
