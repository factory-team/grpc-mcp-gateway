package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type ctxKey struct{}

func newToolMux(opts ...Option) *MCPServeMux {
	mux := NewMCPServeMux(ServerMetadata{Name: "test", Version: "v0"}, opts...)
	mux.RegisterTool(&ToolHandler{
		Name:       "get_thing",
		Title:      "Get thing",
		ReadOnly:   true,
		Idempotent: true,
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			if args["id"] == "boom" {
				return nil, errors.New("upstream said no")
			}
			return map[string]any{"id": args["id"], "caller": ctx.Value(ctxKey{})}, nil
		},
	})
	mux.RegisterTool(&ToolHandler{
		Name:        "delete_thing",
		Destructive: true,
		Handler:     func(context.Context, map[string]any) (any, error) { return "gone", nil },
	})
	mux.RegisterTool(&ToolHandler{
		Name:    "create_thing",
		Handler: func(context.Context, map[string]any) (any, error) { return nil, nil },
	})
	return mux
}

// post sends one JSON-RPC body with the Streamable HTTP headers a client sends.
func post(t *testing.T, h http.Handler, body string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

const meta2026 = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

func headers2026(method, name string) map[string]string {
	h := map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": method}
	if name != "" {
		h["Mcp-Name"] = name
	}
	return h
}

func TestInitializeNegotiatesLegacyVersion(t *testing.T) {
	_, resp := post(t, newToolMux(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil)
	result := resp["result"].(map[string]any)
	if result["protocolVersion"] != "2025-11-25" {
		t.Fatalf("protocolVersion = %v", result["protocolVersion"])
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "test" || info["version"] != "v0" {
		t.Fatalf("serverInfo = %v", info)
	}
	tools := result["capabilities"].(map[string]any)["tools"].(map[string]any)
	if tools["listChanged"] == true {
		t.Fatal("tools.listChanged must not be advertised")
	}
}

func TestDiscoverAdvertises20260728(t *testing.T) {
	rec, resp := post(t, newToolMux(), `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta2026+`}}`, headers2026("server/discover", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
	result := resp["result"].(map[string]any)
	versions := result["supportedVersions"].([]any)
	if versions[0] != "2026-07-28" {
		t.Fatalf("supportedVersions = %v", versions)
	}
	if result["resultType"] != "complete" {
		t.Fatalf("resultType = %v", result["resultType"])
	}
}

func TestToolsListAnnotations(t *testing.T) {
	rec, resp := post(t, newToolMux(), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want a plain JSON response", ct)
	}
	tools := resp["result"].(map[string]any)["tools"].([]any)
	byName := map[string]map[string]any{}
	var order []string
	for _, tool := range tools {
		m := tool.(map[string]any)
		byName[m["name"].(string)] = m
		order = append(order, m["name"].(string))
	}
	if strings.Join(order, ",") != "create_thing,delete_thing,get_thing" {
		t.Fatalf("tools are not in a deterministic order: %v", order)
	}

	get := byName["get_thing"]["annotations"].(map[string]any)
	if get["readOnlyHint"] != true || get["idempotentHint"] != true {
		t.Fatalf("get_thing annotations = %v", get)
	}
	if _, ok := get["destructiveHint"]; ok {
		t.Fatal("destructiveHint must be omitted unless the tool is destructive")
	}
	if byName["delete_thing"]["annotations"].(map[string]any)["destructiveHint"] != true {
		t.Fatalf("delete_thing annotations = %v", byName["delete_thing"]["annotations"])
	}
	if _, ok := byName["create_thing"]["annotations"]; ok {
		t.Fatal("a tool with no hints must not carry annotations")
	}
	if byName["create_thing"]["inputSchema"].(map[string]any)["type"] != "object" {
		t.Fatalf("default inputSchema = %v", byName["create_thing"]["inputSchema"])
	}
}

func TestToolsCall(t *testing.T) {
	// A middleware in front of the mux sets a context value, the way a caller
	// attaches gRPC metadata; the tool handler must see it on both protocols.
	mux := newToolMux()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, "alice")))
	})

	cases := map[string]struct {
		body    string
		headers map[string]string
	}{
		"legacy":     {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_thing","arguments":{"id":"t1"}}}`, nil},
		"2026-07-28": {`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_thing","arguments":{"id":"t1"},` + meta2026 + `}}`, headers2026("tools/call", "get_thing")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec, resp := post(t, h, tc.body, tc.headers)
			if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("status = %d Content-Type = %q body = %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
			}
			result := resp["result"].(map[string]any)
			structured := result["structuredContent"].(map[string]any)
			if structured["id"] != "t1" || structured["caller"] != "alice" {
				t.Fatalf("structuredContent = %v", structured)
			}
			content := result["content"].([]any)[0].(map[string]any)
			if content["type"] != "text" || content["text"] != `{"caller":"alice","id":"t1"}` {
				t.Fatalf("content = %v", content)
			}
			if result["isError"] == true {
				t.Fatal("isError set on a successful call")
			}
		})
	}
}

func TestToolsCallOutputKinds(t *testing.T) {
	mux := newToolMux()
	_, resp := post(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_thing"}}`, nil)
	result := resp["result"].(map[string]any)
	if result["content"].([]any)[0].(map[string]any)["text"] != "gone" {
		t.Fatalf("string output = %v", result)
	}
	if _, ok := result["structuredContent"]; ok {
		t.Fatal("a string output must not carry structuredContent")
	}

	_, resp = post(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_thing"}}`, nil)
	if resp["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "null" {
		t.Fatalf("nil output = %v", resp)
	}
}

func TestToolsCallHandlerErrorIsToolResult(t *testing.T) {
	_, resp := post(t, newToolMux(), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_thing","arguments":{"id":"boom"}}}`, nil)
	if resp["error"] != nil {
		t.Fatalf("handler error surfaced as a protocol error: %v", resp["error"])
	}
	result := resp["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("isError = %v", result["isError"])
	}
	if text := result["content"].([]any)[0].(map[string]any)["text"]; text != "upstream said no" {
		t.Fatalf("error text = %v", text)
	}
}

func TestToolsCallUnknownTool(t *testing.T) {
	_, resp := post(t, newToolMux(), `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`, nil)
	if code := resp["error"].(map[string]any)["code"]; code != float64(-32602) {
		t.Fatalf("unknown tool code = %v", code)
	}
}

func TestRequestLogger(t *testing.T) {
	var seen []string
	mux := newToolMux(WithRequestLogger(func(_ context.Context, req *MCPRequest) {
		name, _ := req.Params["name"].(string)
		seen = append(seen, req.Method+" "+name)
	}))
	post(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	post(t, mux, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delete_thing"}}`, nil)
	if got := strings.Join(seen, "|"); got != "tools/list |tools/call delete_thing" {
		t.Fatalf("logged = %q", got)
	}
}

func TestMaxRequestBodyBytes(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_thing","arguments":{"id":"` + strings.Repeat("x", 4096) + `"}}}`
	if rec, _ := post(t, newToolMux(WithMaxRequestBodyBytes(1024)), body, nil); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status over the limit = %d", rec.Code)
	}
	if rec, _ := post(t, newToolMux(), body, nil); rec.Code != http.StatusOK {
		t.Fatalf("status under the default limit = %d", rec.Code)
	}
}

func TestLocalhostProtection(t *testing.T) {
	send := func(mux *MCPServeMux) int {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Host = "api.example.com"
		// The check reads the address the connection arrived on.
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, loopbackAddr{}))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := send(newToolMux()); code != http.StatusForbidden {
		t.Fatalf("loopback request with a public Host = %d, want 403", code)
	}
	if code := send(newToolMux(WithoutLocalhostProtection())); code != http.StatusOK {
		t.Fatalf("with protection off = %d, want 200", code)
	}
}

type loopbackAddr struct{}

func (loopbackAddr) Network() string { return "tcp" }
func (loopbackAddr) String() string  { return "127.0.0.1:8090" }

func TestClientDisconnectCancelsHandler(t *testing.T) {
	cases := map[string]struct {
		params  string
		headers map[string]string
	}{
		"legacy":     {`{"name":"slow"}`, nil},
		"2026-07-28": {`{"name":"slow",` + meta2026 + `}`, headers2026("tools/call", "slow")},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			started, cancelled := make(chan struct{}), make(chan struct{})
			mux := NewMCPServeMux(ServerMetadata{Name: "test", Version: "v0"})
			mux.RegisterTool(&ToolHandler{
				Name: "slow",
				Handler: func(ctx context.Context, _ map[string]any) (any, error) {
					close(started)
					select {
					case <-ctx.Done():
						close(cancelled)
					case <-time.After(5 * time.Second):
					}
					return nil, ctx.Err()
				},
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			ctx, cancel := context.WithCancel(context.Background())
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+tc.params+`}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			go func() {
				if resp, err := http.DefaultClient.Do(req); err == nil {
					resp.Body.Close()
				}
			}()
			<-started
			cancel()
			select {
			case <-cancelled:
			case <-time.After(3 * time.Second):
				t.Fatal("handler context was not cancelled when the client went away")
			}
		})
	}
}

func TestRegisterToolRejectsBadSchemaAtRegistration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for an input schema that is not an object")
		}
	}()
	NewMCPServeMux(ServerMetadata{}).RegisterTool(&ToolHandler{Name: "bad", InputSchema: map[string]any{"type": "string"}})
}

func Test20260728RequiresMethodHeader(t *testing.T) {
	_, resp := post(t, newToolMux(), `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{`+meta2026+`}}`, map[string]string{"MCP-Protocol-Version": "2026-07-28"})
	if code := resp["error"].(map[string]any)["code"]; code != float64(-32020) {
		t.Fatalf("missing Mcp-Method code = %v", code)
	}
}

func TestRegisterToolAfterFirstRequest(t *testing.T) {
	mux := newToolMux()
	post(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	mux.RegisterTool(&ToolHandler{Name: "late", Handler: func(context.Context, map[string]any) (any, error) { return "ok", nil }})
	_, resp := post(t, mux, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"late"}}`, nil)
	if resp["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "ok" {
		t.Fatalf("late tool = %v", resp)
	}
}
