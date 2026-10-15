package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ifuryst/open-browser-use/internal/wire"
)

func TestMCPInitializeAndListTools(t *testing.T) {
	server := newMCPServer(socketOptions{socketDir: t.TempDir(), timeout: time.Second})
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		"",
	}, "\n")
	var output bytes.Buffer

	if err := server.serve(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	responses := decodeMCPResponses(t, output.Bytes())
	if len(responses) != 2 {
		t.Fatalf("expected initialize and tools/list responses, got %#v", responses)
	}
	initializeResult, _ := responses[0]["result"].(map[string]any)
	if initializeResult["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("expected protocol version %q, got %#v", mcpProtocolVersion, initializeResult["protocolVersion"])
	}
	capabilities, _ := initializeResult["capabilities"].(map[string]any)
	if _, ok := capabilities["tools"].(map[string]any); !ok {
		t.Fatalf("expected tools capability, got %#v", capabilities)
	}

	listResult, _ := responses[1]["result"].(map[string]any)
	tools, _ := listResult["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("expected tools/list to return tools, got %#v", listResult)
	}
	names := map[string]bool{}
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
		if _, ok := tool["inputSchema"].(map[string]any); !ok {
			t.Fatalf("expected tool %q to include inputSchema, got %#v", name, tool)
		}
	}
	for _, name := range []string{"user_tabs", "open_tab", "cdp", "run_action_plan"} {
		if !names[name] {
			t.Fatalf("expected MCP tools to include %q, got %#v", name, names)
		}
	}
}

func TestMCPToolCallInvokesBrowserSocket(t *testing.T) {
	socketDir, err := os.MkdirTemp("/tmp", "obu-mcp-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socketPath := filepath.Join(socketDir, "browser.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	requests := make(chan map[string]any, 1)
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()

		var request map[string]any
		if err := wire.ReadJSON(conn, &request); err != nil {
			serverDone <- err
			return
		}
		requests <- request
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result": []any{
				map[string]any{"id": 321, "title": "Example", "url": "https://example.com"},
			},
		}
		if err := wire.WriteJSON(conn, response); err != nil {
			serverDone <- err
			return
		}
		serverDone <- nil
	}()

	server := newMCPServer(socketOptions{socketPath: socketPath, timeout: time.Second})
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"user_tabs","arguments":{}}}`,
		"",
	}, "\n")
	var output bytes.Buffer

	if err := server.serve(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	browserRequest := <-requests
	if browserRequest["method"] != "getUserTabs" {
		t.Fatalf("expected getUserTabs browser request, got %#v", browserRequest["method"])
	}
	params, _ := browserRequest["params"].(map[string]any)
	if params["session_id"] == "" || params["turn_id"] == "" {
		t.Fatalf("expected MCP tool call to include session and turn ids, got %#v", params)
	}

	responses := decodeMCPResponses(t, output.Bytes())
	if len(responses) != 2 {
		t.Fatalf("expected initialize and tools/call responses, got %#v", responses)
	}
	callResult, _ := responses[1]["result"].(map[string]any)
	if callResult["isError"] != false {
		t.Fatalf("expected successful tool result, got %#v", callResult)
	}
	if _, ok := callResult["structuredContent"].(map[string]any); !ok {
		t.Fatalf("expected structuredContent with browser response, got %#v", callResult)
	}
	content, _ := callResult["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("expected text content mirror, got %#v", callResult["content"])
	}
}

func decodeMCPResponses(t *testing.T, payload []byte) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	responses := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var response map[string]any
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("failed to decode MCP response %q: %v", line, err)
		}
		responses = append(responses, response)
	}
	return responses
}

func TestMCPSessionIDPerAgent(t *testing.T) {
	random := func() string { return "a1b2c3d4" }
	cases := map[string]string{
		"":                         "obu-mcp-a1b2c3d4",
		"   ":                      "obu-mcp-a1b2c3d4",
		"mac/app/fix-login/claude": "berth-mac-app-fix-login-claude",
		"acme.web_2":               "berth-acme.web_2",
		"/weird name/":             "berth-weird-name",
	}
	for in, want := range cases {
		if got := mcpSessionID(in, random); got != want {
			t.Errorf("mcpSessionID(%q) = %q, want %q", in, got, want)
		}
	}
	if long := mcpSessionID(strings.Repeat("x", 200), random); len(long) > len("berth-")+64 {
		t.Errorf("session id not capped: %d chars", len(long))
	}
	if a, b := mcpSessionID("", randomSessionSuffix), mcpSessionID("", randomSessionSuffix); a == b {
		t.Errorf("two servers without BERTH_SESSION got the same session %q", a)
	}
}

func TestMCPCommandKeepsAnExplicitSessionIDFlag(t *testing.T) {
	t.Setenv("BERTH_SESSION", "mac/app/wt/claude")
	cmd := newMCPCommand()
	flag := cmd.Flags().Lookup("session-id")
	if flag == nil || flag.DefValue != defaultMCPSessionID {
		t.Fatalf("expected a --session-id flag defaulting to %q", defaultMCPSessionID)
	}
}

func TestResponseErrorReportsExtensionFailures(t *testing.T) {
	if err := responseError("executeCdp", map[string]any{"id": "cli-1", "result": map[string]any{}}); err != nil {
		t.Fatalf("a result is not an error: %v", err)
	}
	err := responseError("executeCdp", map[string]any{"id": "cli-1", "error": map[string]any{"code": -32000, "message": "navigating to about: URLs is not allowed"}})
	if err == nil || err.Error() != "executeCdp: navigating to about: URLs is not allowed" {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := responseError("attach", map[string]any{"error": "boom"}); err == nil {
		t.Fatal("a non-object error must still fail")
	}
}

// recordingBrowser answers every request on a unix socket with an empty result
// and records the methods asked, across connections.
func recordingBrowser(t *testing.T) (string, func() []string) {
	t.Helper()
	socketDir, err := os.MkdirTemp("/tmp", "obu-mcp-test-")
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
	var mu sync.Mutex
	var methods []string
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				for {
					var request map[string]any
					if err := wire.ReadJSON(conn, &request); err != nil {
						return
					}
					mu.Lock()
					methods = append(methods, fmt.Sprint(request["method"]))
					mu.Unlock()
					if err := wire.WriteJSON(conn, map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{}}); err != nil {
						return
					}
				}
			}()
		}
	}()
	return socketPath, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), methods...)
	}
}

func serveCalls(t *testing.T, socketPath string, tools ...string) {
	t.Helper()
	lines := []string{`{"jsonrpc":"2.0","id":"init","method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`}
	for i, tool := range tools {
		args := `{}`
		if tool == "cdp" {
			args = `{"method":"Runtime.evaluate","params":{"expression":"1"}}`
		}
		if tool == "finalize_tabs" {
			args = `{"keep":[]}`
		}
		lines = append(lines, fmt.Sprintf(`{"jsonrpc":"2.0","id":"c%d","method":"tools/call","params":{"name":%q,"arguments":%s}}`, i, tool, args))
	}
	server := newMCPServer(socketOptions{socketPath: socketPath, timeout: time.Second})
	var output bytes.Buffer
	if err := server.serve(strings.NewReader(strings.Join(lines, "\n")+"\n"), &output); err != nil {
		t.Fatal(err)
	}
}

func TestMCPExitEndsTheTurnAnAgentLeftAttached(t *testing.T) {
	socketPath, methods := recordingBrowser(t)
	serveCalls(t, socketPath, "cdp")
	got := methods()
	if len(got) == 0 || got[len(got)-1] != "turnEnded" {
		t.Fatalf("expected the server to end the turn on exit, browser saw %v", got)
	}
}

func TestMCPExitAfterFinalizeAsksNothingMore(t *testing.T) {
	socketPath, methods := recordingBrowser(t)
	serveCalls(t, socketPath, "cdp", "finalize_tabs")
	got := methods()
	if len(got) == 0 || got[len(got)-1] != "finalizeTabs" {
		t.Fatalf("expected finalizeTabs to be the last request, browser saw %v", got)
	}
}

func TestMCPExitWithoutTabWorkAsksNothing(t *testing.T) {
	socketPath, methods := recordingBrowser(t)
	serveCalls(t, socketPath, "user_tabs")
	for _, m := range methods() {
		if m == "turnEnded" {
			t.Fatalf("a server that touched no tab ended a turn: %v", methods())
		}
	}
}
