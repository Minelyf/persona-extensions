package main

// The tools the assistant and Agents may call (connector "enkeepmemory", capability tools): queue days to be digested
// again with the current daily prompt, and see how far the reruns got. A rerun is a one-off operation, not a setting:
// it reads the user's own sources again inside the extension, sends nothing anywhere and deletes nothing; what it finds
// goes through the same thresholds and review as any day's, and each day runs once per prompt, so asking twice does
// nothing more. rerun_range (the setting) works as before.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

// connectorID is the manifest's connector id.
const connectorID = "enkeepmemory"

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

var rerunTools = []tool{
	{Name: "enkeep_rerun", Description: "Queue past days to be read again with enkeep memory's current extraction prompt " +
		"(for example after the model was down or the prompt changed). Days run one at a time in the background, " +
		"each at most once per prompt version; days already read with the current prompt are skipped. At most 31 days " +
		"per call, only days before today. Returns what was queued; use enkeep_rerun_status for progress.",
		InputSchema: map[string]any{"type": "object", "required": []string{"from", "to"}, "properties": map[string]any{
			"from": map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`, "description": "first day, YYYY-MM-DD"},
			"to":   map[string]any{"type": "string", "pattern": `^\d{4}-\d{2}-\d{2}$`, "description": "last day, YYYY-MM-DD, before today"},
		}}},
	{Name: "enkeep_rerun_status", Description: "Show how far enkeep memory's reruns got: the days queued (by enkeep_rerun " +
		"or the rerun_range setting), which are done with the current prompt, which are waiting, what is running now, " +
		"and whether the passes wait for the model or the assistant's computer.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}}},
}

func (a *app) connector(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	var in struct {
		Contribution string          `json:"contribution"`
		Name         string          `json:"name"`
		Args         json.RawMessage `json:"args"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, &ext.Error{Code: "invalid", Message: "invalid connector parameters"}
		}
	}
	if in.Contribution != "" && in.Contribution != connectorID {
		return nil, ext.NoContribution(in.Contribution)
	}
	switch method {
	case "status":
		return map[string]any{"state": "ok", "account": "enkeep 记忆"}, nil
	case "tools.list":
		return map[string]any{"tools": rerunTools}, nil
	case "tools.call":
		text, err := a.callTool(in.Name, in.Args)
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: err.Error()}
		}
		return map[string]any{"text": text}, nil
	}
	return nil, ext.UnknownMethod("connector." + method)
}

func strictDecode(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func (a *app) callTool(name string, raw json.RawMessage) (string, error) {
	switch name {
	case "enkeep_rerun":
		var in struct{ From, To string }
		if err := strictDecode(raw, &in); err != nil {
			return "", err
		}
		r, err := a.c.QueueRerun(strings.TrimSpace(in.From), strings.TrimSpace(in.To), a.localDay())
		if err != nil {
			return "", err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Queued %d day(s) to read again with the current prompt", len(r.Queued))
		if len(r.Queued) > 0 {
			fmt.Fprintf(&b, ": %s", span(r.Queued))
		}
		b.WriteString(".")
		if len(r.Already) > 0 {
			fmt.Fprintf(&b, " Already done or queued, skipped: %s.", span(r.Already))
		}
		b.WriteString(" They run one at a time in the background; enkeep_rerun_status shows progress.")
		return b.String(), nil
	case "enkeep_rerun_status":
		if err := strictDecode(raw, &struct{}{}); err != nil {
			return "", err
		}
		p := a.c.RerunStatus()
		var b strings.Builder
		fmt.Fprintf(&b, "Prompt %s: %d day(s) done, %d waiting.", p.Prompt, len(p.Done), len(p.Waiting))
		if len(p.Waiting) > 0 {
			fmt.Fprintf(&b, " Waiting: %s.", span(p.Waiting))
		}
		a.mu.Lock()
		running := a.running
		a.mu.Unlock()
		if running != "" {
			fmt.Fprintf(&b, " Running now: %s.", running)
		}
		if p.Paused != "" {
			fmt.Fprintf(&b, " Paused: %s.", p.Paused)
		}
		return b.String(), nil
	}
	return "", errors.New("unknown tool " + name)
}

// localDay is the local day in the curator's time zone.
func (a *app) localDay() string {
	now := time.Now
	if a.c.Now != nil {
		now = a.c.Now
	}
	return now().In(a.c.Loc).Format("2006-01-02")
}

// span says a list of days briefly: "2026-10-29..2026-11-13 (16)" when they follow each other, else the list.
func span(days []string) string {
	if len(days) == 0 {
		return ""
	}
	consecutive := true
	for i := 1; i < len(days); i++ {
		a, _ := time.Parse("2006-01-02", days[i-1])
		if a.AddDate(0, 0, 1).Format("2006-01-02") != days[i] {
			consecutive = false
			break
		}
	}
	if consecutive && len(days) > 2 {
		return fmt.Sprintf("%s..%s (%d)", days[0], days[len(days)-1], len(days))
	}
	return strings.Join(days, ", ")
}
