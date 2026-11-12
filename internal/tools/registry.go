package tools

import (
	"context"
	"fmt"
	"sync"

	"github.com/metabbe3/GoAgent/internal/config"
	"github.com/metabbe3/GoAgent/internal/kb"
	"github.com/metabbe3/GoAgent/internal/llm"
)

// ToolHandler is a function that executes a tool with parsed JSON arguments.
type ToolHandler func(ctx context.Context, argsJSON string) (string, error)

// Tool represents a registered agent tool.
type Tool struct {
	Definition llm.ToolDefinition
	Handler    ToolHandler
}

// Registry manages the set of available tools and dispatches execution.
type Registry struct {
	mu         sync.RWMutex
	tools      map[string]Tool
	allowShell bool
	storeDir   string
	kbIndex    *kb.Index
}

// NewRegistry initializes the registry with all standard tools.
func NewRegistry(cfg *config.Config) *Registry {
	allowShell := false
	if cfg != nil {
		allowShell = cfg.AllowShell
	}

	r := &Registry{
		tools:      make(map[string]Tool),
		allowShell: allowShell,
	}

	r.registerAll()
	return r
}

// SetStoreDir allows overriding pattern store directory (useful in tests).
func (r *Registry) SetStoreDir(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.storeDir = dir
}

// SetAllowShell configures whether unrestricted shell execution is permitted.
func (r *Registry) SetAllowShell(allow bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allowShell = allow
}

// Register adds a tool to the registry.
func (r *Registry) Register(def llm.ToolDefinition, handler ToolHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[def.Function.Name] = Tool{
		Definition: def,
		Handler:    handler,
	}
}

// Has reports whether a tool with the given name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.tools[name]
	return exists
}

// ResultCap returns the maximum byte length for a tool's output.
func (r *Registry) ResultCap(name string) int {
	return 8000
}

// KBIndex returns the attached knowledge base index, if any.
func (r *Registry) KBIndex() *kb.Index {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.kbIndex
}

// GetDefinitions returns the OpenAI tool definitions for all registered tools.
func (r *Registry) GetDefinitions() []llm.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	defs := make([]llm.ToolDefinition, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Definition)
	}
	return defs
}

// Execute runs the named tool with the given JSON arguments.
func (r *Registry) Execute(ctx context.Context, name, argsJSON string) (string, error) {
	r.mu.RLock()
	tool, exists := r.tools[name]
	r.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("unknown tool: %s", name)
	}

	if argsJSON == "" {
		argsJSON = "{}"
	}

	result, err := tool.Handler(ctx, argsJSON)
	if err != nil {
		return fmt.Sprintf("Error executing %s: %v", name, err), nil
	}
	return result, nil
}

func (r *Registry) registerAll() {
	r.registerFilesTools()
	r.registerShellTools()
	r.registerLogsTools()
	r.registerWebTools()
	r.registerDocumentTools()
	r.registerGenerateTools("data")
}

// Helper to marshal JSON schema map
func schema(properties map[string]any, required []string) any {
	s := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
