package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ifuryst/open-browser-use/internal/host"
	"github.com/spf13/cobra"
)

const mcpProtocolVersion = "2025-06-18"

type mcpServer struct {
	options socketOptions
	runner  *actionRunner
	// extensionChecked: as Shipyard's chrome plugin, the server made sure
	// Chrome runs the extension this binary ships (see ensureShippedExtension).
	extensionChecked bool
	// shipyardProfiles: use only the Chrome profiles Shipyard's settings
	// allow (--shipyard-profiles); chosenProfile is the one use_profile picked.
	shipyardProfiles bool
	chosenProfile    string
	// attached: a tool may have left the debugger attached to a tab since
	// the last finalize_tabs or turn_ended. Chrome shows "started debugging
	// this browser" on every tab while one is, so the server ends the turn
	// itself when it exits (see endTurn).
	attached bool
	ended    sync.Once
}

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func newMCPCommand() *cobra.Command {
	options := socketOptions{sessionID: defaultMCPSessionID}
	var shipyardProfiles bool
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP stdio server for Open Browser Use",
		Long: `Run an MCP stdio server for Open Browser Use.

The server exposes browser tools over newline-delimited JSON-RPC on stdin/stdout.
It is intended for agent runtimes that can launch local MCP servers, for example
with the command "obu mcp".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("session-id") {
				options.sessionID = mcpSessionID(os.Getenv("BERTH_SESSION"), randomSessionSuffix)
			}
			server := newMCPServer(options)
			if shipyardProfiles {
				if options.profile != "" || options.socketPath != "" {
					return errors.New("--shipyard-profiles picks the profile itself: drop --profile and --socket")
				}
				server.shipyardProfiles = true
			}
			// An agent runtime that stops the server sends a signal rather
			// than closing stdin first: end the turn then too.
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			go func() {
				<-signals
				server.endTurn()
				os.Exit(0)
			}()
			return server.serve(os.Stdin, os.Stdout)
		},
	}
	addSocketFlags(cmd, &options)
	cmd.Flags().BoolVar(&shipyardProfiles, "shipyard-profiles", false, "use only the Chrome profiles Shipyard's chrome plugin settings allow ($BERTH_PLUGIN_SETTINGS)")
	return cmd
}

// mcpSessionID gives each agent its own browser session, so agents running
// side by side get their own tab group and finalize_tabs only closes their
// own tabs (the extension keys tabs by session). Under Shipyard it follows
// the agent's session ($BERTH_SESSION), so it survives the MCP server being
// restarted; otherwise it is unique to this server process.
func mcpSessionID(berthSession string, random func() string) string {
	if name := sessionIDSafe(berthSession); name != "" {
		return "berth-" + name
	}
	return defaultMCPSessionID + "-" + random()
}

// sessionIDSafe keeps a session name to letters, digits and ._-, at most 64
// characters, for use as a session id.
func sessionIDSafe(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func randomSessionSuffix() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf[:])
}

func newMCPServer(options socketOptions) *mcpServer {
	if options.socketDir == "" {
		options.socketDir = host.DefaultSocketDir
	}
	if options.timeout == 0 {
		options.timeout = 10 * time.Second
	}
	if options.sessionID == "" {
		options.sessionID = defaultMCPSessionID
	}
	return &mcpServer{
		options: options,
		runner:  newActionRunner(options),
	}
}

func (server *mcpServer) serve(reader io.Reader, writer io.Writer) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	encoder := json.NewEncoder(writer)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		responses := server.handleLine([]byte(line))
		for _, response := range responses {
			if response == nil {
				continue
			}
			if err := encoder.Encode(response); err != nil {
				return err
			}
		}
	}
	err := scanner.Err()
	server.endTurn()
	return err
}

// endTurn detaches the debugger from this session's tabs, once, when the
// agent is done with the server (its stdin closed, or it was told to stop)
// without having called finalize_tabs or turn_ended. Tabs stay open; only
// the debugger is let go. Best effort: the browser may be gone already.
func (server *mcpServer) endTurn() {
	server.ended.Do(func() {
		if !server.attached {
			return
		}
		_, _ = server.invoke("turnEnded", map[string]any{})
	})
}

// leavesAttached says whether a tool may attach the debugger to a tab.
func leavesAttached(tool string) bool {
	switch tool {
	case "ping", "info", "tabs", "user_tabs", "history", "name_session", "profiles", "use_profile", "finalize_tabs", "turn_ended":
		return false
	}
	return true
}

func (server *mcpServer) handleLine(payload []byte) []map[string]any {
	if len(payload) > 0 && payload[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(payload, &batch); err != nil {
			return []map[string]any{mcpErrorResponse(nil, -32700, "Parse error")}
		}
		responses := make([]map[string]any, 0, len(batch))
		for _, item := range batch {
			if response := server.handleMessage(item); response != nil {
				responses = append(responses, response)
			}
		}
		return responses
	}
	if response := server.handleMessage(payload); response != nil {
		return []map[string]any{response}
	}
	return nil
}

func (server *mcpServer) handleMessage(payload []byte) map[string]any {
	var request mcpRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return mcpErrorResponse(nil, -32700, "Parse error")
	}
	if request.Method == "" {
		return mcpErrorResponse(request.ID, -32600, "Invalid request")
	}
	if request.ID == nil {
		return nil
	}

	switch request.Method {
	case "initialize":
		return mcpResultResponse(request.ID, server.initializeResult(request.Params))
	case "ping":
		return mcpResultResponse(request.ID, map[string]any{})
	case "tools/list":
		return mcpResultResponse(request.ID, map[string]any{"tools": server.tools()})
	case "tools/call":
		result, err := server.callTool(request.Params)
		if err != nil {
			return mcpErrorResponse(request.ID, -32602, err.Error())
		}
		return mcpResultResponse(request.ID, result)
	default:
		return mcpErrorResponse(request.ID, -32601, "Method not found")
	}
}

func (server *mcpServer) initializeResult(params json.RawMessage) map[string]any {
	protocolVersion := mcpProtocolVersion
	var value struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && json.Unmarshal(params, &value) == nil {
		if value.ProtocolVersion == "2024-11-05" || value.ProtocolVersion == "2025-03-26" || value.ProtocolVersion == mcpProtocolVersion {
			protocolVersion = value.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{
				"listChanged": false,
			},
		},
		"serverInfo": map[string]any{
			"name":    "open-browser-use",
			"version": version,
		},
	}
}

func mcpResultResponse(id any, result any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
}

func mcpErrorResponse(id any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": mcpError{
			Code:    code,
			Message: message,
		},
	}
}

func mcpTools() []mcpTool {
	return []mcpTool{
		{
			Name:        "ping",
			Title:       "Ping Browser Backend",
			Description: "Check that the Open Browser Use browser backend is reachable.",
			InputSchema: emptyObjectSchema(),
		},
		{
			Name:        "info",
			Title:       "Get Browser Backend Info",
			Description: "Return Open Browser Use browser backend metadata.",
			InputSchema: emptyObjectSchema(),
		},
		{
			Name:        "tabs",
			Title:       "List Session Tabs",
			Description: "List tabs currently managed by the active Open Browser Use session.",
			InputSchema: emptyObjectSchema(),
		},
		{
			Name:        "user_tabs",
			Title:       "List User Tabs",
			Description: "List visible Chrome tabs from the user's current browser profile.",
			InputSchema: emptyObjectSchema(),
		},
		{
			Name:        "history",
			Title:       "Search Chrome History",
			Description: "Search the user's Chrome history.",
			InputSchema: objectSchema(map[string]any{
				"query": stringSchema("Search query. Leave empty to list recent history."),
				"limit": map[string]any{
					"type":        "integer",
					"description": "Maximum result count.",
					"minimum":     1,
					"default":     100,
				},
				"from": stringSchema("Optional ISO start date."),
				"to":   stringSchema("Optional ISO end date."),
			}, nil),
		},
		{
			Name:        "open_tab",
			Title:       "Open Browser Tab",
			Description: "Open a new managed browser tab and optionally navigate it to a URL.",
			InputSchema: objectSchema(map[string]any{
				"url": stringSchema("Optional URL to navigate after creating the tab."),
			}, nil),
		},
		{
			Name:        "claim_tab",
			Title:       "Claim Existing Tab",
			Description: "Claim an existing Chrome tab by id for the current browser session.",
			InputSchema: objectSchema(map[string]any{
				"tab_id": integerSchema("Chrome tab id to claim."),
			}, []string{"tab_id"}),
		},
		{
			Name:        "navigate",
			Title:       "Navigate Tab",
			Description: "Navigate a managed tab. If tab_id is omitted, uses the current tab from a prior open_tab or claim_tab call.",
			InputSchema: objectSchema(map[string]any{
				"tab_id": integerSchema("Optional Chrome tab id. Defaults to the current tab."),
				"url":    stringSchema("URL to navigate to."),
			}, []string{"url"}),
		},
		{
			Name:        "wait_load",
			Title:       "Wait For Page Load",
			Description: "Wait for a managed tab to reach a load state.",
			InputSchema: objectSchema(map[string]any{
				"tab_id": integerSchema("Optional Chrome tab id. Defaults to the current tab."),
				"state": map[string]any{
					"type":        "string",
					"description": "Load state.",
					"enum":        []string{"load", "domcontentloaded"},
					"default":     "load",
				},
			}, nil),
		},
		{
			Name:        "page_info",
			Title:       "Read Page Info",
			Description: "Read title, URL, readyState, and body text from a managed tab.",
			InputSchema: objectSchema(map[string]any{
				"tab_id": integerSchema("Optional Chrome tab id. Defaults to the current tab."),
			}, nil),
		},
		{
			Name:        "snapshot",
			Title:       "Snapshot Page",
			Description: "Compact text outline of the current page: interactive elements as [ref] role \"name\" value, plus headings, paragraphs and table rows in the viewport. Use query to search the whole page instead of scrolling. Prefer this over cdp.",
			InputSchema: objectSchema(map[string]any{
				"tab_id":    integerSchema("Optional Chrome tab id. Defaults to the current tab."),
				"query":     stringSchema("Optional case-insensitive text to find anywhere on the page."),
				"full_page": map[string]any{"type": "boolean", "description": "Include content outside the viewport.", "default": false},
			}, nil),
		},
		{
			Name:        "act",
			Title:       "Act On Element",
			Description: "Act on elements by ref from snapshot, then return the new snapshot. Actions: click, fill (value), select (option value or text), check, uncheck, press (key, default Enter), focus. Batch several steps, e.g. a whole form, with actions:[{ref,action,value}].",
			InputSchema: objectSchema(map[string]any{
				"tab_id": integerSchema("Optional Chrome tab id. Defaults to the current tab."),
				"ref":    stringSchema("Element ref from snapshot."),
				"action": map[string]any{"type": "string", "enum": actActions},
				"value":  stringSchema("Text for fill, option for select, key for press."),
				"actions": map[string]any{
					"type":        "array",
					"description": "Steps run in order; stops at the first error.",
					"items": objectSchema(map[string]any{
						"ref":    map[string]any{"type": "string"},
						"action": map[string]any{"type": "string", "enum": actActions},
						"value":  map[string]any{"type": "string"},
					}, []string{"ref", "action"}),
				},
			}, nil),
		},
		{
			Name:        "scroll",
			Title:       "Scroll Page",
			Description: "Scroll the current page by most of a viewport and return the new snapshot.",
			InputSchema: objectSchema(map[string]any{
				"tab_id":    integerSchema("Optional Chrome tab id. Defaults to the current tab."),
				"direction": map[string]any{"type": "string", "enum": []string{"down", "up"}, "default": "down"},
			}, nil),
		},
		{
			Name:        "cdp",
			Title:       "Run CDP Command",
			Description: "Run a raw Chrome DevTools Protocol command against a managed tab. Low level; prefer snapshot/act.",
			InputSchema: objectSchema(map[string]any{
				"tab_id": integerSchema("Optional Chrome tab id. Defaults to the current tab."),
				"method": stringSchema("Chrome DevTools Protocol method, for example Runtime.evaluate."),
				"params": map[string]any{
					"type":        "object",
					"description": "Chrome DevTools Protocol command parameters.",
					"default":     map[string]any{},
				},
			}, []string{"method"}),
		},
		{
			Name:        "move_mouse",
			Title:       "Move Browser Cursor",
			Description: "Move the browser cursor overlay inside a managed tab.",
			InputSchema: objectSchema(map[string]any{
				"tab_id": integerSchema("Optional Chrome tab id. Defaults to the current tab."),
				"x":      numberSchema("Viewport x coordinate."),
				"y":      numberSchema("Viewport y coordinate."),
			}, []string{"x", "y"}),
		},
		{
			Name:        "wait_file_chooser",
			Title:       "Wait For File Chooser",
			Description: "Wait for a managed tab to open a file chooser.",
			InputSchema: objectSchema(map[string]any{
				"tab_id": integerSchema("Optional Chrome tab id. Defaults to the current tab."),
			}, nil),
		},
		{
			Name:        "set_file_chooser_files",
			Title:       "Set File Chooser Files",
			Description: "Set files for an intercepted browser file chooser.",
			InputSchema: objectSchema(map[string]any{
				"file_chooser_id": stringSchema("File chooser id returned by wait_file_chooser."),
				"files": map[string]any{
					"type":        "array",
					"description": "Local file paths to set.",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
				},
			}, []string{"file_chooser_id", "files"}),
		},
		{
			Name:        "finalize_tabs",
			Title:       "Finalize Tabs",
			Description: "Finalize managed browser tabs. Pass keep entries only for handoff or deliverable tabs.",
			InputSchema: objectSchema(map[string]any{
				"keep": map[string]any{
					"type":        "array",
					"description": "Tabs to keep, for example [{\"tabId\":123,\"status\":\"handoff\"}].",
					"default":     []any{},
					"items":       map[string]any{"type": "object"},
				},
			}, nil),
		},
		{
			Name:        "name_session",
			Title:       "Name Browser Session",
			Description: "Set the Chrome tab group title for the active browser session.",
			InputSchema: objectSchema(map[string]any{
				"name": stringSchema("Session group title, normally '<short task> - OBU'."),
			}, []string{"name"}),
		},
		{
			Name:        "turn_ended",
			Title:       "End Browser Turn",
			Description: "Tell Open Browser Use that the current browser-control turn has ended.",
			InputSchema: emptyObjectSchema(),
		},
		{
			Name:        "call",
			Title:       "Call Browser RPC",
			Description: "Send an unrestricted Open Browser Use JSON-RPC request to the browser backend.",
			InputSchema: objectSchema(map[string]any{
				"method": stringSchema("Open Browser Use backend method."),
				"params": map[string]any{
					"type":        "object",
					"description": "Backend method parameters.",
					"default":     map[string]any{},
				},
			}, []string{"method"}),
		},
		{
			Name:        "run_action_plan",
			Title:       "Run Action Plan",
			Description: "Run a line-oriented Open Browser Use action plan using the same session and current tab.",
			InputSchema: objectSchema(map[string]any{
				"script": stringSchema("Line-oriented action plan script."),
			}, []string{"script"}),
		},
	}
}

func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	}
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringSchema(description string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": description,
	}
}

func integerSchema(description string) map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": description,
	}
}

func numberSchema(description string) map[string]any {
	return map[string]any{
		"type":        "number",
		"description": description,
	}
}

func (server *mcpServer) callTool(params json.RawMessage) (map[string]any, error) {
	var request struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(params) == 0 {
		return nil, errors.New("tools/call requires params")
	}
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, fmt.Errorf("invalid tools/call params: %w", err)
	}
	if request.Name == "" {
		return nil, errors.New("tools/call requires name")
	}
	if request.Arguments == nil {
		request.Arguments = map[string]any{}
	}
	if server.shipyardProfiles {
		switch request.Name {
		case "profiles":
			return server.profilesTool()
		case "use_profile":
			out, err := server.useProfileTool(request.Arguments)
			if err != nil {
				return mcpToolErrorResult(err.Error()), nil
			}
			return mcpToolResult(out)
		}
		if err := server.selectShipyardProfile(); err != nil {
			return mcpToolErrorResult(err.Error()), nil
		}
	}
	if err := server.checkPinned(); err != nil {
		return mcpToolErrorResult(err.Error()), nil
	}
	if err := server.ensureShippedExtension(); err != nil {
		return mcpToolErrorResult(err.Error()), nil
	}
	if leavesAttached(request.Name) {
		server.attached = true
	}
	output, err := server.runTool(request.Name, request.Arguments)
	if err == nil && (request.Name == "finalize_tabs" || request.Name == "turn_ended") {
		server.attached = false
	}
	if err != nil {
		return mcpToolErrorResult(err.Error()), nil
	}
	if text, ok := output.(textOutput); ok {
		return mcpTextToolResult(string(text)), nil
	}
	return mcpToolResult(output)
}

func (server *mcpServer) runTool(name string, arguments map[string]any) (any, error) {
	switch name {
	case "ping":
		return server.invoke("ping", map[string]any{})
	case "info":
		return server.invoke("getInfo", map[string]any{})
	case "tabs":
		return server.invoke("getTabs", map[string]any{})
	case "user_tabs":
		return server.invoke("getUserTabs", map[string]any{})
	case "history":
		return server.runHistoryTool(arguments)
	case "open_tab":
		return server.runOpenTabTool(arguments)
	case "claim_tab":
		tabID, err := requiredIntArg(arguments, "tab_id")
		if err != nil {
			return nil, err
		}
		response, _, err := server.runner.invoke("claimUserTab", map[string]any{"tabId": tabID})
		if err == nil {
			server.runner.currentTabID = tabID
		}
		return response, err
	case "navigate":
		return server.runNavigateTool(arguments)
	case "wait_load":
		return server.runWaitLoadTool(arguments)
	case "page_info":
		return server.runPageInfoTool(arguments)
	case "snapshot", "act", "scroll":
		return server.runSnapshotTool(name, arguments)
	case "cdp":
		return server.runCDPTool(arguments)
	case "move_mouse":
		return server.runMoveMouseTool(arguments)
	case "wait_file_chooser":
		return server.runWaitFileChooserTool(arguments)
	case "set_file_chooser_files":
		return server.runSetFileChooserFilesTool(arguments)
	case "finalize_tabs":
		keep, err := optionalArrayArg(arguments, "keep")
		if err != nil {
			return nil, err
		}
		return server.invoke("finalizeTabs", map[string]any{"keep": keep})
	case "name_session":
		name, err := requiredStringArg(arguments, "name")
		if err != nil {
			return nil, err
		}
		return server.invoke("nameSession", map[string]any{"name": name})
	case "turn_ended":
		return server.invoke("turnEnded", map[string]any{})
	case "call":
		method, err := requiredStringArg(arguments, "method")
		if err != nil {
			return nil, err
		}
		params, err := optionalObjectArg(arguments, "params")
		if err != nil {
			return nil, err
		}
		return server.invoke(method, params)
	case "run_action_plan":
		script, err := requiredStringArg(arguments, "script")
		if err != nil {
			return nil, err
		}
		return server.runner.run(script)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func (server *mcpServer) invoke(method string, params map[string]any) (map[string]any, error) {
	response, _, err := server.runner.invoke(method, params)
	return response, err
}

// checkPinned refuses to guess a browser for an agent: when the server was
// started with no --socket, --browser or --profile and more than one
// extension is connected, "the active one" may be any of them (a banking
// profile included).
func (server *mcpServer) checkPinned() error {
	o := server.runner.options
	if o.socketPath != "" || strings.TrimSpace(o.browser) != "" || strings.TrimSpace(o.profile) != "" {
		return nil
	}
	if connected := probeConnectedProfiles(o.socketDir); len(connected) > 1 {
		return ambiguousBrowserError(connected)
	}
	return nil
}

// ensureShippedExtension runs once per server when it is Shipyard's chrome
// plugin ($BERTH_PLUGIN_DIR): after a plugin update Chrome may still run the
// old extension, so reload it before the agent's first call.
func (server *mcpServer) ensureShippedExtension() error {
	if server.extensionChecked || os.Getenv("BERTH_PLUGIN_DIR") == "" {
		return nil
	}
	p, err := resolveShipyardPaths("")
	if err != nil {
		return err
	}
	if ok, why := extensionIntact(p.ExtensionDir); !ok {
		return fmt.Errorf("the chrome plugin is not set up for this version (%s): finish its setup in Shipyard → Settings → Plugins", why)
	}
	if err := reloadShipyardExtension(p, 15*time.Second); err != nil {
		return err
	}
	server.extensionChecked = true
	return nil
}

func (server *mcpServer) runHistoryTool(arguments map[string]any) (any, error) {
	limit := 100
	if value, ok, err := optionalIntArg(arguments, "limit"); err != nil {
		return nil, err
	} else if ok {
		limit = value
	}
	params := map[string]any{
		"query": optionalStringArg(arguments, "query"),
		"limit": limit,
	}
	if from := optionalStringArg(arguments, "from"); from != "" {
		params["from"] = from
	}
	if to := optionalStringArg(arguments, "to"); to != "" {
		params["to"] = to
	}
	return server.invoke("getUserHistory", params)
}

func (server *mcpServer) runOpenTabTool(arguments map[string]any) (any, error) {
	args := []string{}
	if url := optionalStringArg(arguments, "url"); url != "" {
		args = append(args, url)
	}
	response, _, err := server.runner.runOpenTabAction(args)
	return response, err
}

func (server *mcpServer) runNavigateTool(arguments map[string]any) (any, error) {
	url, err := requiredStringArg(arguments, "url")
	if err != nil {
		return nil, err
	}
	args := []string{url}
	if tabID, ok, err := optionalIntArg(arguments, "tab_id"); err != nil {
		return nil, err
	} else if ok {
		args = append([]string{"--tab-id", strconv.Itoa(tabID)}, args...)
	}
	response, _, err := server.runner.runNavigateAction(args)
	return response, err
}

func (server *mcpServer) runWaitLoadTool(arguments map[string]any) (any, error) {
	args := []string{}
	if state := optionalStringArg(arguments, "state"); state != "" {
		args = append(args, "--state", state)
	}
	if tabID, ok, err := optionalIntArg(arguments, "tab_id"); err != nil {
		return nil, err
	} else if ok {
		args = append([]string{"--tab-id", strconv.Itoa(tabID)}, args...)
	}
	response, _, err := server.runner.runWaitLoadAction(args)
	return response, err
}

func (server *mcpServer) runPageInfoTool(arguments map[string]any) (any, error) {
	args := []string{}
	if tabID, ok, err := optionalIntArg(arguments, "tab_id"); err != nil {
		return nil, err
	} else if ok {
		args = append(args, "--tab-id", strconv.Itoa(tabID))
	}
	response, _, err := server.runner.runPageInfoAction(args)
	return response, err
}

func (server *mcpServer) runSnapshotTool(name string, arguments map[string]any) (any, error) {
	tabID := server.runner.currentTabID
	if value, ok, err := optionalIntArg(arguments, "tab_id"); err != nil {
		return nil, err
	} else if ok {
		tabID = value
	}
	if tabID <= 0 {
		return nil, errors.New(name + " requires tab_id or a previous open_tab/claim_tab call")
	}
	switch name {
	case "snapshot":
		opts := map[string]any{}
		if query := optionalStringArg(arguments, "query"); query != "" {
			opts["query"] = query
		}
		if fullPage, ok := arguments["full_page"].(bool); ok && fullPage {
			opts["full_page"] = true
		}
		return server.runner.runSnapshotAction(tabID, opts)
	case "act":
		steps, err := parseActSteps(arguments)
		if err != nil {
			return nil, err
		}
		return server.runner.runActSteps(tabID, steps)
	default:
		direction := optionalStringArg(arguments, "direction")
		if direction != "up" {
			direction = "down"
		}
		return server.runner.runScrollAction(tabID, direction)
	}
}

func (server *mcpServer) runCDPTool(arguments map[string]any) (any, error) {
	method, err := requiredStringArg(arguments, "method")
	if err != nil {
		return nil, err
	}
	params, err := optionalObjectArg(arguments, "params")
	if err != nil {
		return nil, err
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	args := []string{method, string(paramsJSON)}
	if tabID, ok, err := optionalIntArg(arguments, "tab_id"); err != nil {
		return nil, err
	} else if ok {
		args = append([]string{"--tab-id", strconv.Itoa(tabID)}, args...)
	}
	response, _, err := server.runner.runCDPAction(args)
	return response, err
}

func (server *mcpServer) runMoveMouseTool(arguments map[string]any) (any, error) {
	x, err := requiredFloatArg(arguments, "x")
	if err != nil {
		return nil, err
	}
	y, err := requiredFloatArg(arguments, "y")
	if err != nil {
		return nil, err
	}
	args := []string{strconv.FormatFloat(x, 'f', -1, 64), strconv.FormatFloat(y, 'f', -1, 64)}
	if tabID, ok, err := optionalIntArg(arguments, "tab_id"); err != nil {
		return nil, err
	} else if ok {
		args = append([]string{"--tab-id", strconv.Itoa(tabID)}, args...)
	}
	response, _, err := server.runner.runMoveMouseAction(args)
	return response, err
}

func (server *mcpServer) runWaitFileChooserTool(arguments map[string]any) (any, error) {
	args := []string{}
	if tabID, ok, err := optionalIntArg(arguments, "tab_id"); err != nil {
		return nil, err
	} else if ok {
		args = append(args, "--tab-id", strconv.Itoa(tabID))
	}
	response, _, err := server.runner.runWaitFileChooserAction(args)
	return response, err
}

func (server *mcpServer) runSetFileChooserFilesTool(arguments map[string]any) (any, error) {
	fileChooserID, err := requiredStringArg(arguments, "file_chooser_id")
	if err != nil {
		return nil, err
	}
	files, err := requiredStringArrayArg(arguments, "files")
	if err != nil {
		return nil, err
	}
	args := append([]string{fileChooserID}, files...)
	response, _, err := server.runner.runSetFileChooserFilesAction(args)
	return response, err
}

func mcpToolResult(output any) (map[string]any, error) {
	payload, err := json.Marshal(output)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": string(payload),
			},
		},
		"structuredContent": output,
		"isError":           false,
	}, nil
}

func mcpToolErrorResult(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{
				"type": "text",
				"text": message,
			},
		},
		"isError": true,
	}
}

func requiredStringArg(arguments map[string]any, name string) (string, error) {
	value := optionalStringArg(arguments, name)
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func optionalStringArg(arguments map[string]any, name string) string {
	if value, ok := arguments[name].(string); ok {
		return value
	}
	return ""
}

func requiredIntArg(arguments map[string]any, name string) (int, error) {
	value, ok, err := optionalIntArg(arguments, name)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func optionalIntArg(arguments map[string]any, name string) (int, bool, error) {
	raw, ok := arguments[name]
	if !ok || raw == nil {
		return 0, false, nil
	}
	switch value := raw.(type) {
	case float64:
		intValue := int(value)
		if float64(intValue) != value {
			return 0, false, fmt.Errorf("%s must be an integer", name)
		}
		return intValue, true, nil
	case int:
		return value, true, nil
	default:
		return 0, false, fmt.Errorf("%s must be an integer", name)
	}
}

func requiredFloatArg(arguments map[string]any, name string) (float64, error) {
	raw, ok := arguments[name]
	if !ok || raw == nil {
		return 0, fmt.Errorf("%s is required", name)
	}
	switch value := raw.(type) {
	case float64:
		return value, nil
	case int:
		return float64(value), nil
	default:
		return 0, fmt.Errorf("%s must be a number", name)
	}
}

func optionalObjectArg(arguments map[string]any, name string) (map[string]any, error) {
	raw, ok := arguments[name]
	if !ok || raw == nil {
		return map[string]any{}, nil
	}
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	return value, nil
}

func optionalArrayArg(arguments map[string]any, name string) ([]any, error) {
	raw, ok := arguments[name]
	if !ok || raw == nil {
		return []any{}, nil
	}
	value, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	return value, nil
}

func requiredStringArrayArg(arguments map[string]any, name string) ([]string, error) {
	raw, ok := arguments[name]
	if !ok || raw == nil {
		return nil, fmt.Errorf("%s is required", name)
	}
	values, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", name)
	}
	out := make([]string, 0, len(values))
	for _, rawValue := range values {
		value, ok := rawValue.(string)
		if !ok || value == "" {
			return nil, fmt.Errorf("%s must contain only non-empty strings", name)
		}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s must contain at least one file", name)
	}
	return out, nil
}

// tools are the MCP tools this server offers.
func (server *mcpServer) tools() []mcpTool {
	tools := mcpTools()
	if server.shipyardProfiles {
		tools = append(tools,
			mcpTool{
				Name:        "profiles",
				Title:       "List Chrome Profiles",
				Description: "List the Chrome profiles the user allowed agents to use, which is in use, and whether each is open.",
				InputSchema: emptyObjectSchema(),
			},
			mcpTool{
				Name:        "use_profile",
				Title:       "Use Chrome Profile",
				Description: "Switch to another allowed Chrome profile (by name or folder from `profiles`). Its sign-ins are the user's: use it only for the task the user asked for.",
				InputSchema: objectSchema(map[string]any{
					"profile": stringSchema("Profile name or folder, from `profiles`."),
				}, []string{"profile"}),
			},
		)
	}
	return tools
}

// selectShipyardProfile points the server at an allowed profile before a
// browser call: the one use_profile picked, else "Shipyard Agent", else the
// only allowed one. It reads the settings every call, so a profile the user
// turns off stops being used at once.
func (server *mcpServer) selectShipyardProfile() error {
	p, err := resolveShipyardPaths("")
	if err != nil {
		return err
	}
	allowed := allowedAgentProfiles(p)
	if len(allowed) == 0 {
		return errors.New("no Chrome profile is turned on for agents: ask the user to turn one on in Shipyard → Settings → Plugins → Chrome")
	}
	pick := ""
	for _, ap := range allowed {
		if server.chosenProfile != "" && ap.Dir == server.chosenProfile {
			pick = ap.Dir
		}
	}
	if server.chosenProfile != "" && pick == "" {
		return fmt.Errorf("the user turned off the Chrome profile %q for agents; call profiles to see the ones you may use", server.chosenProfile)
	}
	if pick == "" {
		for _, ap := range allowed {
			if ap.Dir == shipyardProfile {
				pick = ap.Dir
			}
		}
	}
	if pick == "" && len(allowed) == 1 {
		pick = allowed[0].Dir
	}
	if pick == "" {
		names := make([]string, 0, len(allowed))
		for _, ap := range allowed {
			names = append(names, fmt.Sprintf("%q", ap.Name))
		}
		return fmt.Errorf("several Chrome profiles are allowed (%s): call use_profile to pick one", strings.Join(names, ", "))
	}
	if selector := profileDirSelectorPrefix + pick; server.runner.options.profile != selector {
		server.runner.options.profile = selector
		server.options.profile = selector
		server.runner.currentTabID = 0
	}
	if server.runner.options.browser == "" {
		server.runner.options.browser = "chrome"
		server.options.browser = "chrome"
	}
	return nil
}

func (server *mcpServer) useProfileTool(arguments map[string]any) (any, error) {
	want, err := requiredStringArg(arguments, "profile")
	if err != nil {
		return nil, err
	}
	p, err := resolveShipyardPaths("")
	if err != nil {
		return nil, err
	}
	for _, ap := range allowedAgentProfiles(p) {
		if strings.EqualFold(ap.Dir, want) || strings.EqualFold(ap.Name, want) {
			server.chosenProfile = ap.Dir
			if err := server.selectShipyardProfile(); err != nil {
				return nil, err
			}
			return map[string]any{"profile": ap.Name, "dir": ap.Dir}, nil
		}
	}
	return nil, fmt.Errorf("%q is not a Chrome profile the user allowed for agents; call profiles to see them", want)
}

func (server *mcpServer) profilesTool() (map[string]any, error) {
	p, err := resolveShipyardPaths("")
	if err != nil {
		return mcpToolErrorResult(err.Error()), nil
	}
	open := map[string]bool{}
	for _, c := range connectedShipyardExtensions(p) {
		open[c.Directory] = true
	}
	type row struct {
		Name  string `json:"name"`
		Dir   string `json:"dir"`
		InUse bool   `json:"inUse"`
		Open  bool   `json:"open"`
	}
	rows := []row{}
	for _, ap := range allowedAgentProfiles(p) {
		rows = append(rows, row{Name: ap.Name, Dir: ap.Dir, InUse: profileDirSelectorPrefix+ap.Dir == server.runner.options.profile, Open: open[ap.Dir]})
	}
	return mcpToolResult(map[string]any{"profiles": rows})
}
