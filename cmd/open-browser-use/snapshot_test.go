package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifuryst/open-browser-use/internal/wire"
)

// fakeBrowser answers extension socket requests; reply decides each result.
type fakeBrowser struct {
	mu       sync.Mutex
	requests []map[string]any
}

func startFakeBrowser(t *testing.T, reply func(method string, params map[string]any) any) (string, *fakeBrowser) {
	t.Helper()
	socketDir, err := os.MkdirTemp("/tmp", "obu-snap-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "browser.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	browser := &fakeBrowser{}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				for {
					var request map[string]any
					if err := wire.ReadJSON(conn, &request); err != nil {
						return
					}
					method, _ := request["method"].(string)
					params, _ := request["params"].(map[string]any)
					browser.mu.Lock()
					browser.requests = append(browser.requests, request)
					browser.mu.Unlock()
					wire.WriteJSON(conn, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": reply(method, params)})
				}
			}(conn)
		}
	}()
	return socketPath, browser
}

func (browser *fakeBrowser) cdpMethods() []string {
	browser.mu.Lock()
	defer browser.mu.Unlock()
	var methods []string
	for _, request := range browser.requests {
		params, _ := request["params"].(map[string]any)
		if method, ok := params["method"].(string); ok {
			methods = append(methods, method)
		}
	}
	return methods
}

// evaluateReply mimics Runtime.evaluate: readyState checks report complete, the
// snapshot script returns value for its op.
func evaluateReply(values map[string]any) func(string, map[string]any) any {
	return func(method string, params map[string]any) any {
		if method != "executeCdp" || params["method"] != "Runtime.evaluate" {
			return map[string]any{}
		}
		commandParams, _ := params["commandParams"].(map[string]any)
		expression, _ := commandParams["expression"].(string)
		if expression == "document.readyState" {
			return map[string]any{"result": map[string]any{"type": "string", "value": "complete"}}
		}
		for op, value := range values {
			if strings.Contains(expression, `)("`+op+`",`) {
				return map[string]any{"result": map[string]any{"value": value}}
			}
		}
		return map[string]any{"result": map[string]any{"value": nil}}
	}
}

func callMCPTool(t *testing.T, socketPath, call string) map[string]any {
	t.Helper()
	server := newMCPServer(socketOptions{socketPath: socketPath, timeout: 2 * time.Second})
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		call,
		"",
	}, "\n")
	var output bytes.Buffer
	if err := server.serve(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	responses := decodeMCPResponses(t, output.Bytes())
	result, _ := responses[len(responses)-1]["result"].(map[string]any)
	return result
}

func TestSnapshotExpressionEmbedsScriptAndArguments(t *testing.T) {
	expression, err := snapshotExpression("act", map[string]any{"ref": "3", "action": "click"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(expression, "(") || !strings.Contains(expression, "function obuSnapshot") {
		t.Fatalf("expression should wrap the embedded script, got prefix %q", expression[:40])
	}
	if !strings.HasSuffix(expression, `)("act",{"action":"click","ref":"3"})`) {
		t.Fatalf("unexpected call suffix: %q", expression[len(expression)-60:])
	}
}

func TestRuntimeEvaluateValueReportsPageExceptions(t *testing.T) {
	_, err := runtimeEvaluateValue(map[string]any{"result": map[string]any{
		"exceptionDetails": map[string]any{
			"text":      "Uncaught",
			"exception": map[string]any{"description": "Error: ref 9 not found on this page; take a new snapshot\n    at <anonymous>:1:2"},
		},
	}})
	if err == nil || err.Error() != "Error: ref 9 not found on this page; take a new snapshot" {
		t.Fatalf("expected first line of the page exception, got %v", err)
	}
}

func TestMCPListsSnapshotTools(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range mcpTools() {
		names[tool.Name] = true
	}
	for _, name := range []string{"snapshot", "act", "scroll"} {
		if !names[name] {
			t.Fatalf("expected %q tool", name)
		}
	}
}

func TestSnapshotToolReturnsPlainTextOnly(t *testing.T) {
	outline := "Form — http://localhost/form\nscroll 0/0px\n[1] textbox \"Full name\" value=\"\""
	socketPath, _ := startFakeBrowser(t, evaluateReply(map[string]any{"snapshot": outline}))

	result := callMCPTool(t, socketPath, `{"jsonrpc":"2.0","id":"s","method":"tools/call","params":{"name":"snapshot","arguments":{"tab_id":7}}}`)

	if result["isError"] != false {
		t.Fatalf("expected success, got %#v", result)
	}
	if _, ok := result["structuredContent"]; ok {
		t.Fatal("snapshot must not duplicate its text in structuredContent")
	}
	content, _ := result["content"].([]any)
	first, _ := content[0].(map[string]any)
	if first["text"] != outline {
		t.Fatalf("expected raw outline text, got %#v", first["text"])
	}
}

func TestActClickDispatchesRealMouseEventsAndReturnsNewSnapshot(t *testing.T) {
	socketPath, browser := startFakeBrowser(t, evaluateReply(map[string]any{
		"act":      map[string]any{"mouse": map[string]any{"x": 10.0, "y": 20.0}},
		"snapshot": "Thanks — http://localhost/submit\nscroll 0/0px\nh1: Submission received",
	}))

	result := callMCPTool(t, socketPath, `{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"act","arguments":{"tab_id":7,"ref":"3","action":"click"}}}`)

	content, _ := result["content"].([]any)
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.HasPrefix(text, "ok: click [3]\nThanks") {
		t.Fatalf("expected action note followed by new snapshot, got %q", text)
	}
	mouseEvents := 0
	for _, method := range browser.cdpMethods() {
		if method == "Input.dispatchMouseEvent" {
			mouseEvents++
		}
	}
	if mouseEvents != 3 {
		t.Fatalf("expected move, press and release events, got %d", mouseEvents)
	}
}

func TestActPageErrorIsReturnedAsToolError(t *testing.T) {
	socketPath, _ := startFakeBrowser(t, func(method string, params map[string]any) any {
		if method == "executeCdp" && params["method"] == "Runtime.evaluate" {
			return map[string]any{"exceptionDetails": map[string]any{
				"exception": map[string]any{"description": "Error: ref 42 not found on this page; take a new snapshot"},
			}}
		}
		return map[string]any{}
	})

	result := callMCPTool(t, socketPath, `{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"act","arguments":{"tab_id":7,"ref":"42","action":"click"}}}`)

	if result["isError"] != true {
		t.Fatalf("expected tool error for a stale ref, got %#v", result)
	}
}
