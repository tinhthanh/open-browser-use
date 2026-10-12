package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// snapshotScript is a self-contained page function, evaluated through the
// existing executeCdp path, so the extension needs no new permissions.
//
//go:embed snapshot.js
var snapshotScript string

// textOutput marks tool output that should be returned to MCP clients as plain
// text only. Snapshot text is already compact; echoing it again as JSON in
// structuredContent would double what the model reads.
type textOutput string

const actSettleDelay = 300 * time.Millisecond

var actActions = []string{"click", "fill", "select", "check", "uncheck", "press", "focus"}

const actUsage = `act needs ref and action (click|fill|select|check|uncheck|press|focus); fill/select need value. ` +
	`Example: {"ref":"3","action":"fill","value":"text"} or {"actions":[{"ref":"1","action":"fill","value":"a"},{"ref":"5","action":"click"}]}`

type actStep struct {
	ref    string
	action string
	value  string
}

// scalarString accepts the string or number forms models send for refs/values.
func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	}
	return ""
}

func parseActStep(raw map[string]any) (actStep, error) {
	step := actStep{ref: scalarString(raw["ref"]), action: strings.ToLower(scalarString(raw["action"])), value: scalarString(raw["value"])}
	if step.ref == "" || step.action == "" {
		return step, errors.New(actUsage)
	}
	valid := false
	for _, action := range actActions {
		valid = valid || action == step.action
	}
	if !valid {
		return step, fmt.Errorf("unknown action %q. %s", step.action, actUsage)
	}
	if (step.action == "fill" || step.action == "select") && raw["value"] == nil {
		return step, fmt.Errorf("%s on [%s] needs value. %s", step.action, step.ref, actUsage)
	}
	return step, nil
}

func parseActSteps(arguments map[string]any) ([]actStep, error) {
	if rawSteps, ok := arguments["actions"].([]any); ok && len(rawSteps) > 0 {
		steps := make([]actStep, 0, len(rawSteps))
		for index, rawStep := range rawSteps {
			object, _ := rawStep.(map[string]any)
			step, err := parseActStep(object)
			if err != nil {
				return nil, fmt.Errorf("actions[%d]: %w", index, err)
			}
			steps = append(steps, step)
		}
		return steps, nil
	}
	step, err := parseActStep(arguments)
	if err != nil {
		return nil, err
	}
	return []actStep{step}, nil
}

// keyDefinitions maps the keys agents commonly press to CDP key event fields.
var keyDefinitions = map[string]struct {
	code    string
	keyCode int
	text    string
}{
	"Enter":      {"Enter", 13, "\r"},
	"Tab":        {"Tab", 9, ""},
	"Escape":     {"Escape", 27, ""},
	"Backspace":  {"Backspace", 8, ""},
	"ArrowDown":  {"ArrowDown", 40, ""},
	"ArrowUp":    {"ArrowUp", 38, ""},
	"ArrowLeft":  {"ArrowLeft", 37, ""},
	"ArrowRight": {"ArrowRight", 39, ""},
	"PageDown":   {"PageDown", 34, ""},
	"PageUp":     {"PageUp", 33, ""},
}

func snapshotExpression(op string, opts map[string]any) (string, error) {
	opJSON, err := json.Marshal(op)
	if err != nil {
		return "", err
	}
	optsJSON, err := json.Marshal(opts)
	if err != nil {
		return "", err
	}
	return "(" + snapshotScript + ")(" + string(opJSON) + "," + string(optsJSON) + ")", nil
}

// evaluatePage runs the snapshot script and returns its JSON value, turning
// page exceptions into Go errors.
func (runner *actionRunner) evaluatePage(tabID int, op string, opts map[string]any) (any, error) {
	expression, err := snapshotExpression(op, opts)
	if err != nil {
		return nil, err
	}
	response, _, err := runner.invoke("executeCdp", map[string]any{
		"target": map[string]any{"tabId": tabID},
		"method": "Runtime.evaluate",
		"commandParams": map[string]any{
			"expression":    expression,
			"returnByValue": true,
			"awaitPromise":  true,
		},
	})
	if err != nil {
		return nil, err
	}
	return runtimeEvaluateValue(response)
}

func runtimeEvaluateValue(response map[string]any) (any, error) {
	result, _ := response["result"].(map[string]any)
	if details, ok := result["exceptionDetails"].(map[string]any); ok {
		message, _ := details["text"].(string)
		if exception, ok := details["exception"].(map[string]any); ok {
			if description, ok := exception["description"].(string); ok && description != "" {
				message = strings.SplitN(description, "\n", 2)[0]
			}
		}
		return nil, errors.New(strings.TrimPrefix(message, "Uncaught "))
	}
	cdpResult, _ := result["result"].(map[string]any)
	return cdpResult["value"], nil
}

func (runner *actionRunner) cdp(tabID int, method string, params map[string]any) error {
	_, _, err := runner.invoke("executeCdp", map[string]any{
		"target":        map[string]any{"tabId": tabID},
		"method":        method,
		"commandParams": params,
	})
	return err
}

func (runner *actionRunner) snapshotText(tabID int, opts map[string]any) (string, error) {
	value, err := runner.evaluatePage(tabID, "snapshot", opts)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", errors.New("snapshot returned no text")
	}
	return text, nil
}

func (runner *actionRunner) runSnapshotAction(tabID int, opts map[string]any) (textOutput, error) {
	if err := runner.attach(tabID); err != nil {
		return "", err
	}
	runner.waitReady(tabID, runner.options.timeout)
	text, err := runner.snapshotText(tabID, opts)
	return textOutput(text), err
}

// runActSteps performs ref-based actions in order and returns one snapshot of
// the result, so the agent sees the effect without another call. On a failed
// step it reports what already happened instead of discarding that progress.
func (runner *actionRunner) runActSteps(tabID int, steps []actStep) (textOutput, error) {
	if err := runner.attach(tabID); err != nil {
		return "", err
	}
	notes := make([]string, 0, len(steps))
	for index, step := range steps {
		if index > 0 {
			time.Sleep(actSettleDelay / 3)
		}
		note, err := runner.runActStep(tabID, step)
		if err != nil {
			if index == 0 {
				return "", err
			}
			notes = append(notes, fmt.Sprintf("FAILED: %s [%s]: %v (remaining steps skipped)", step.action, step.ref, err))
			break
		}
		notes = append(notes, "ok: "+note)
	}
	return runner.settleAndSnapshot(tabID, strings.Join(notes, "\n"))
}

func (runner *actionRunner) runActStep(tabID int, step actStep) (string, error) {
	ref, action, value := step.ref, step.action, step.value
	plan, err := runner.evaluatePage(tabID, "act", map[string]any{"ref": ref, "action": action, "value": value})
	if err != nil {
		return "", err
	}
	result, _ := plan.(map[string]any)
	note := fmt.Sprintf("%s [%s]", action, ref)
	switch {
	case result["mouse"] != nil:
		point, _ := result["mouse"].(map[string]any)
		x, _ := point["x"].(float64)
		y, _ := point["y"].(float64)
		if err := runner.mouseClick(tabID, x, y); err != nil {
			return "", err
		}
	case result["key"] != nil:
		key, _ := result["key"].(string)
		if err := runner.pressKey(tabID, key); err != nil {
			return "", err
		}
		note = fmt.Sprintf("press %s on [%s]", key, ref)
	case result["done"] != nil:
		note = fmt.Sprintf("%s [%s]: %v", action, ref, result["done"])
	}
	return note, nil
}

func (runner *actionRunner) runScrollAction(tabID int, direction string) (textOutput, error) {
	if err := runner.attach(tabID); err != nil {
		return "", err
	}
	if _, err := runner.evaluatePage(tabID, "scroll", map[string]any{"direction": direction}); err != nil {
		return "", err
	}
	return runner.settleAndSnapshot(tabID, "ok: scroll "+direction)
}

// settleAndSnapshot waits briefly for the action's effect (including a page
// navigation) and returns the result lines followed by the new viewport snapshot.
func (runner *actionRunner) settleAndSnapshot(tabID int, header string) (textOutput, error) {
	time.Sleep(actSettleDelay)
	runner.waitReady(tabID, 5*time.Second)
	text, err := runner.snapshotText(tabID, map[string]any{})
	if err != nil {
		// A navigation can tear down the context mid-read; retry once on the new page.
		time.Sleep(actSettleDelay)
		runner.waitReady(tabID, 5*time.Second)
		if text, err = runner.snapshotText(tabID, map[string]any{}); err != nil {
			return textOutput(header + "\n(snapshot unavailable: " + err.Error() + ")"), nil
		}
	}
	return textOutput(header + "\n" + text), nil
}

// waitReady polls document.readyState until "complete" or the timeout. Errors
// are ignored: during navigation evaluate fails until the new document exists.
func (runner *actionRunner) waitReady(tabID int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		response, _, err := runner.invoke("executeCdp", map[string]any{
			"target":        map[string]any{"tabId": tabID},
			"method":        "Runtime.evaluate",
			"commandParams": map[string]any{"expression": "document.readyState", "returnByValue": true},
		})
		if err == nil && runtimeEvaluateString(response) == "complete" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (runner *actionRunner) mouseClick(tabID int, x, y float64) error {
	for _, event := range []map[string]any{
		{"type": "mouseMoved", "x": x, "y": y},
		{"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1},
		{"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1},
	} {
		if err := runner.cdp(tabID, "Input.dispatchMouseEvent", event); err != nil {
			return err
		}
	}
	return nil
}

func (runner *actionRunner) pressKey(tabID int, key string) error {
	definition, ok := keyDefinitions[key]
	if !ok {
		return fmt.Errorf("unsupported key %q", key)
	}
	down := map[string]any{"type": "keyDown", "key": key, "code": definition.code, "windowsVirtualKeyCode": definition.keyCode}
	if definition.text != "" {
		down["text"] = definition.text
	}
	if err := runner.cdp(tabID, "Input.dispatchKeyEvent", down); err != nil {
		return err
	}
	return runner.cdp(tabID, "Input.dispatchKeyEvent", map[string]any{
		"type": "keyUp", "key": key, "code": definition.code, "windowsVirtualKeyCode": definition.keyCode,
	})
}

func mcpTextToolResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	}
}
