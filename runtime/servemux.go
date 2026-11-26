package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPServeMux registers gRPC-backed tools and serves them as a stateless MCP
// server over Streamable HTTP. The protocol itself (JSON-RPC, version
// negotiation, the 2026-07-28 stateless flow and the legacy initialize
// handshake) is handled by the official MCP Go SDK.
type MCPServeMux struct {
	mu            sync.RWMutex
	tools         map[string]*ToolHandler
	resources     map[string]*Resource
	metadata      ServerMetadata
	requestLogger RequestLogger

	maxRequestBodyBytes        int64
	disableLocalhostProtection bool

	// The SDK server is built on the first request, so the capabilities it
	// advertises reflect everything registered at startup. Until then tools
	// and resources are also added to probe, so an invalid registration
	// panics at startup and not inside the first request.
	once    sync.Once
	probe   *mcp.Server
	server  *mcp.Server
	handler http.Handler
}

// requestContextKey carries the HTTP request's context to tool handlers. The
// SDK only ties a handler's context to the request on protocol 2026-07-28, so
// for older clients the mux links the two itself.
type requestContextKey struct{}

// ToolHandler handles an MCP tool call by invoking a gRPC method
type ToolHandler struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any
	ReadOnly    bool
	Idempotent  bool
	Destructive bool
	UI          *ToolUI
	Handler     func(ctx context.Context, args map[string]any) (any, error)
}

// ServerMetadata contains server information
type ServerMetadata struct {
	Name    string
	Version string
}

// MCPRequest is the view of an incoming MCP request a RequestLogger receives.
type MCPRequest struct {
	Method string
	Params map[string]any
}

// RequestLogger handles MCP request logging. It sees every request the
// protocol layer accepts; requests rejected at the HTTP layer (bad Accept or
// Content-Type, oversized body) never reach it.
type RequestLogger func(ctx context.Context, req *MCPRequest)

// Option configures the MCPServeMux.
type Option func(*MCPServeMux)

// WithRequestLogger sets the request logger for MCP requests.
func WithRequestLogger(logger RequestLogger) Option {
	return func(mux *MCPServeMux) {
		if logger != nil {
			mux.requestLogger = logger
		}
	}
}

// WithMaxRequestBodyBytes sets the largest request body the server reads.
// Zero keeps the SDK default (mcp.DefaultMaxRequestBodyBytes, 4 MiB); a
// negative value removes the limit.
func WithMaxRequestBodyBytes(n int64) Option {
	return func(mux *MCPServeMux) { mux.maxRequestBodyBytes = n }
}

// WithoutLocalhostProtection turns off the SDK's DNS-rebinding check, which
// answers 403 to a request that arrives over loopback with a non-localhost
// Host header. Use it only when a trusted local proxy fronts the server.
func WithoutLocalhostProtection() Option {
	return func(mux *MCPServeMux) { mux.disableLocalhostProtection = true }
}

// NewMCPServeMux creates a new stateless MCP request multiplexer
func NewMCPServeMux(metadata ServerMetadata, opts ...Option) *MCPServeMux {
	mux := &MCPServeMux{
		tools:     make(map[string]*ToolHandler),
		resources: make(map[string]*Resource),
		metadata:  metadata,
		probe:     mcp.NewServer(&mcp.Implementation{Name: metadata.Name, Version: metadata.Version}, nil),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(mux)
		}
	}
	return mux
}

// RegisterTool registers a new tool handler
func (mux *MCPServeMux) RegisterTool(tool *ToolHandler) {
	mux.mu.Lock()
	defer mux.mu.Unlock()
	mux.tools[tool.Name] = tool
	if mux.server != nil {
		addTool(mux.server, tool)
		return
	}
	addTool(mux.probe, tool)
}

// ServeHTTP implements http.Handler.
func (mux *MCPServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	mux.once.Do(mux.build)
	ctx := context.WithValue(r.Context(), requestContextKey{}, r.Context())
	mux.handler.ServeHTTP(w, r.WithContext(ctx))
}

func (mux *MCPServeMux) build() {
	mux.mu.Lock()
	defer mux.mu.Unlock()

	caps := &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}
	if len(mux.resources) > 0 {
		caps.Resources = &mcp.ResourceCapabilities{}
	}
	if mux.hasAppResources() {
		caps.AddExtension(AppsExtensionID, map[string]any{"mimeTypes": []string{AppResourceMIMEType}})
	}

	server := mcp.NewServer(
		&mcp.Implementation{Name: mux.metadata.Name, Version: mux.metadata.Version},
		&mcp.ServerOptions{Capabilities: caps},
	)
	if mux.requestLogger != nil {
		server.AddReceivingMiddleware(mux.logRequests)
	}
	for _, tool := range mux.tools {
		addTool(server, tool)
	}
	for _, res := range mux.resources {
		addResource(server, res)
	}
	mux.server, mux.probe = server, nil

	mux.handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			Stateless:                  true,
			JSONResponse:               true,
			MaxRequestBodyBytes:        mux.maxRequestBodyBytes,
			DisableLocalhostProtection: mux.disableLocalhostProtection,
		},
	)
}

func (mux *MCPServeMux) logRequests(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		var params map[string]any
		if b, err := json.Marshal(req.GetParams()); err == nil {
			_ = json.Unmarshal(b, &params)
		}
		mux.requestLogger(ctx, &MCPRequest{Method: method, Params: params})
		return next(ctx, method, req)
	}
}

// addTool registers tool on an SDK server.
func addTool(server *mcp.Server, tool *ToolHandler) {
	var schema any = tool.InputSchema
	if tool.InputSchema == nil {
		schema = DefaultInputSchema()
	}
	t := &mcp.Tool{
		Name:        tool.Name,
		Title:       tool.Title,
		Description: tool.Description,
		InputSchema: schema,
		Meta:        toolMeta(tool.UI),
	}
	if tool.ReadOnly || tool.Idempotent || tool.Destructive {
		t.Annotations = &mcp.ToolAnnotations{
			ReadOnlyHint:   tool.ReadOnly,
			IdempotentHint: tool.Idempotent,
		}
		if tool.Destructive {
			t.Annotations.DestructiveHint = &tool.Destructive
		}
	}
	server.AddTool(t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Cancel the call when the client goes away, on every protocol version.
		if reqCtx, ok := ctx.Value(requestContextKey{}).(context.Context); ok {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			defer context.AfterFunc(reqCtx, cancel)()
		}
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
		}
		output, err := tool.Handler(ctx, args)
		if err != nil {
			// A failed call is a tool result the model can read and correct,
			// not a protocol error.
			res := &mcp.CallToolResult{}
			res.SetError(err)
			return res, nil
		}
		return toolResult(output), nil
	})
}

func toolResult(output any) *mcp.CallToolResult {
	res := &mcp.CallToolResult{}
	var text string
	switch v := output.(type) {
	case string:
		text = v
	case []byte:
		text = string(v)
	case nil:
		text = "null"
	default:
		if b, err := json.Marshal(v); err == nil {
			text = string(b)
			res.StructuredContent = v
		} else {
			text = fmt.Sprintf("%v", v)
		}
	}
	res.Content = []mcp.Content{&mcp.TextContent{Text: text}}
	return res
}

// DefaultInputSchema provides a permissive object schema for tool inputs.
func DefaultInputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": true,
	}
}
