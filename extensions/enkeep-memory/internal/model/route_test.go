package model

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// fakeHub answers model.complete by the model a call names: each model's answer is a function of the call.
type fakeHub struct {
	answers map[string]func() (wire.ModelCompleteResult, error) // by model; "" is a call without models
	calls   []string                                            // the model each call named ("" none)
	limits  []int                                               // each call's timeout_seconds
}

func (f *fakeHub) call(_ context.Context, params any, out *wire.ModelCompleteResult) error {
	b, _ := json.Marshal(params)
	var p struct {
		Models  []string `json:"models"`
		Timeout int      `json:"timeout_seconds"`
	}
	_ = json.Unmarshal(b, &p)
	m := ""
	if len(p.Models) > 0 {
		m = p.Models[0]
	}
	f.calls = append(f.calls, m)
	f.limits = append(f.limits, p.Timeout)
	a, ok := f.answers[m]
	if !ok {
		return &ext.Error{Code: wire.ModelErrorCodeModelUnavailable, Message: "model.complete: no model " + m}
	}
	r, err := a()
	*out = r
	return err
}

func ok(model string) func() (wire.ModelCompleteResult, error) {
	return func() (wire.ModelCompleteResult, error) {
		return wire.ModelCompleteResult{Text: `{"a":1}`, JSON: json.RawMessage(`{"a":1}`), Model: model, StopReason: "end_turn"}, nil
	}
}

func fails(code, msg string) func() (wire.ModelCompleteResult, error) {
	return func() (wire.ModelCompleteResult, error) {
		return wire.ModelCompleteResult{}, &ext.Error{Code: code, Message: msg}
	}
}

func testRoute(t *testing.T) (*Route, string) {
	dir := t.TempDir()
	return &Route{Primary: []string{"flash"}, Long: "big", Short: "small", LongAbove: 5000,
		Stats: OpenStats(dir, time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))}, dir
}

var shortReq = Request{Step: "triage", System: "sys", User: "short input", MaxTokens: 1000, Schema: json.RawMessage(`{"type":"object"}`)}
var longReq = Request{Step: "daily", System: "sys", User: strings.Repeat("中", 6000), MaxTokens: 1000, Schema: json.RawMessage(`{"type":"object"}`)}

func TestRoutePrimaryAnswers(t *testing.T) {
	rt, _ := testRoute(t)
	f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){"flash": ok("flash"), "small": ok("small")}}
	resp, err := Hub{Route: rt, call: f.call}.Complete(context.Background(), shortReq)
	if err != nil || resp.Model != "flash" || resp.Text != `{"a":1}` {
		t.Fatalf("%+v %v", resp, err)
	}
	if strings.Join(f.calls, ",") != "flash" {
		t.Fatalf("calls %v", f.calls)
	}
	s := rt.Stats.Snapshot()
	if s.Calls != 1 || s.FellBack != 0 || s.Models["flash"].Answered != 1 {
		t.Fatalf("stats %+v", s)
	}
}

// Every kind of failure of the primary model falls back, to the short or the long model by the call's size.
func TestRouteFallsBackByLength(t *testing.T) {
	for name, primary := range map[string]func() (wire.ModelCompleteResult, error){
		ReasonError:   fails(wire.ModelErrorCodeModelFailed, "model.complete: 502 upstream"),
		ReasonTimeout: fails(wire.ModelErrorCodeTimeout, "model.complete: timed out"),
		ReasonRefused: fails(wire.ModelErrorCodeModelRefused, "model.complete: declined"),
		ReasonFormat:  fails(wire.ModelErrorCodeInvalidOutput, "model.complete: the reply does not match json_schema"),
		ReasonEmpty: func() (wire.ModelCompleteResult, error) {
			return wire.ModelCompleteResult{Text: " ", Model: "flash", StopReason: "end_turn"}, nil
		},
	} {
		for _, c := range []struct {
			r    Request
			want string
		}{{shortReq, "small"}, {longReq, "big"}} {
			rt, _ := testRoute(t)
			f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){"flash": primary, "small": ok("small"), "big": ok("big")}}
			resp, err := Hub{Route: rt, call: f.call}.Complete(context.Background(), c.r)
			if err != nil || resp.Model != c.want {
				t.Fatalf("%s %s: %+v %v", name, c.r.Step, resp, err)
			}
			if strings.Join(f.calls, ",") != "flash,"+c.want {
				t.Fatalf("%s: calls %v", name, f.calls)
			}
			s := rt.Stats.Snapshot()
			if s.Calls != 1 || s.FellBack != 1 || s.Models["flash"].Failures[name] != 1 || s.Models[c.want].AsBackup != 1 {
				t.Fatalf("%s: stats %+v", name, s)
			}
			if got := s.Recent[0]; got.Answered != c.want || !got.FellBack || got.Attempts[0].Reason != name {
				t.Fatalf("%s: recent %+v", name, got)
			}
		}
	}
}

func TestEstimate(t *testing.T) {
	// 6000 Chinese characters, "sys" and the schema in ASCII, 1000 for the answer.
	if got := Estimate(longReq); got < 7000 || got > 7020 {
		t.Fatalf("long %d", got)
	}
	if got := Estimate(Request{User: strings.Repeat("a", 400)}); got != 100+4096 {
		t.Fatalf("ascii %d", got)
	}
	rt, _ := testRoute(t)
	if rt.Size(Estimate(shortReq)) != SizeShort || rt.Size(Estimate(longReq)) != SizeLong {
		t.Fatal("sizes")
	}
}

// A hub that does not take "models" turns the call down; it goes the old way, and so do the next ones.
func TestRouteHubWithoutModels(t *testing.T) {
	rt, _ := testRoute(t)
	unknown := fails(wire.ModelErrorCodeInvalidParams, `model.complete: json: unknown field "models"`)
	f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){"flash": unknown, "small": unknown, "": ok("purpose-model")}}
	h := Hub{Route: rt, call: f.call}
	for i := 0; i < 2; i++ {
		resp, err := h.Complete(context.Background(), shortReq)
		if err != nil || resp.Model != "purpose-model" {
			t.Fatalf("%+v %v", resp, err)
		}
	}
	if strings.Join(f.calls, ",") != "flash,," {
		t.Fatalf("calls %q", f.calls)
	}
	if s := rt.Stats.Snapshot(); s.Calls != 2 || s.Legacy != 2 {
		t.Fatalf("stats %+v", s)
	}
}

// No model of the route is on the connection: the purpose's own model answers.
func TestRouteNoneOffered(t *testing.T) {
	rt, _ := testRoute(t)
	f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){"": ok("purpose-model")}}
	resp, err := Hub{Route: rt, call: f.call}.Complete(context.Background(), longReq)
	if err != nil || resp.Model != "purpose-model" || strings.Join(f.calls, ",") != "flash,big," {
		t.Fatalf("%+v %v %q", resp, err, f.calls)
	}
}

// The hub cannot run a model for the extension at all: nothing else is tried, the run waits as before.
func TestRouteAssistantOffline(t *testing.T) {
	rt, _ := testRoute(t)
	f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){
		"flash": fails(wire.ModelErrorCodeAssistantOffline, "offline"), "small": ok("small")}}
	_, err := Hub{Route: rt, call: f.call}.Complete(context.Background(), shortReq)
	if !errors.Is(err, ErrUnavailable) || len(f.calls) != 1 {
		t.Fatalf("%v %v", err, f.calls)
	}
}

// Every model paused or refusing: the caller gets the error it handles (the day waits; the batch is split).
func TestRouteAllFail(t *testing.T) {
	rt, _ := testRoute(t)
	f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){
		"flash": fails(wire.ModelErrorCodeModelPaused, "paused"), "small": fails(wire.ModelErrorCodeModelPaused, "paused")}}
	if _, err := (Hub{Route: rt, call: f.call}).Complete(context.Background(), shortReq); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("paused: %v", err)
	}
	f = &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){
		"flash": fails(wire.ModelErrorCodeModelFailed, "ai: the model declined to answer"), "small": fails(wire.ModelErrorCodeModelRefused, "declined")}}
	if _, err := (Hub{Route: rt, call: f.call}).Complete(context.Background(), shortReq); !errors.Is(err, ErrRefused) {
		t.Fatalf("refused: %v", err)
	}
	if s := rt.Stats.Snapshot(); s.Failed != 2 {
		t.Fatalf("stats %+v", s)
	}
}

// No model matches the schema: the last reply the hub returned anyway goes to the caller's own checks, as before.
func TestRouteKeepsNonconformingReply(t *testing.T) {
	rt, _ := testRoute(t)
	bad := func(text string) func() (wire.ModelCompleteResult, error) {
		return func() (wire.ModelCompleteResult, error) {
			data, _ := json.Marshal(wire.ModelInvalidOutput{Problem: "missing", Text: text})
			return wire.ModelCompleteResult{}, &ext.Error{Code: wire.ModelErrorCodeInvalidOutput, Message: "bad", Data: data}
		}
	}
	f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){"flash": bad(`{"x":1}`), "small": bad(`{"y":2}`)}}
	resp, err := Hub{Route: rt, call: f.call}.Complete(context.Background(), shortReq)
	if err != nil || resp.Text != `{"y":2}` || resp.Nonconforming == "" || resp.Model != "small" {
		t.Fatalf("%+v %v", resp, err)
	}
}

// Without a route the call names no model, as before.
func TestNoRouteIsTheOldCall(t *testing.T) {
	f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){"": ok("purpose-model")}}
	resp, err := Hub{call: f.call}.Complete(context.Background(), shortReq)
	if err != nil || resp.Model != "purpose-model" || strings.Join(f.calls, ",") != "" {
		t.Fatalf("%+v %v %q", resp, err, f.calls)
	}
	if (&Route{}).Enabled() || (*Route)(nil).Enabled() {
		t.Fatal("an empty route is off")
	}
}

// The stats outlive a restart, every call is a line of the calls log, and the summary says the counts.
func TestStatsPersist(t *testing.T) {
	rt, dir := testRoute(t)
	f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){
		"flash": fails(wire.ModelErrorCodeTimeout, "timed out"), "small": ok("small")}}
	for i := 0; i < 3; i++ {
		if _, err := (Hub{Route: rt, call: f.call}).Complete(context.Background(), shortReq); err != nil {
			t.Fatal(err)
		}
	}
	s := OpenStats(dir, time.Now())
	if s.Calls != 3 || s.FellBack != 3 || s.Since != "2026-11-18T00:00:00Z" || s.Models["flash"].Failures[ReasonTimeout] != 3 {
		t.Fatalf("reopened %+v", s)
	}
	sum := s.Summary()
	for _, w := range []string{"3, answered by a fallback 3 (100.0%)", "flash: 3 attempt(s), 0 answered", "timeout 3", "small: 3 attempt(s), 3 answered (3 as fallback)"} {
		if !strings.Contains(sum, w) {
			t.Errorf("summary lacks %q:\n%s", w, sum)
		}
	}
	file, err := os.Open(filepath.Join(dir, CallsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	n := 0
	for sc := bufio.NewScanner(file); sc.Scan(); n++ {
		var c Call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil || c.Answered != "small" || c.Attempts[0].Reason != ReasonTimeout {
			t.Fatalf("line %d: %s %v", n, sc.Text(), err)
		}
	}
	if n != 3 {
		t.Fatalf("lines %d", n)
	}
}

// A primary model's attempt has the route's shorter limit; the fallback keeps the step's.
func TestRoutePrimaryTimeout(t *testing.T) {
	rt, _ := testRoute(t)
	rt.PrimaryTimeout = 120
	f := &fakeHub{answers: map[string]func() (wire.ModelCompleteResult, error){
		"flash": fails(wire.ModelErrorCodeTimeout, "timed out"), "small": ok("small")}}
	if _, err := (Hub{Route: rt, call: f.call}).Complete(context.Background(), shortReq); err != nil {
		t.Fatal(err)
	}
	if len(f.limits) != 2 || f.limits[0] != 120 || f.limits[1] != 300 {
		t.Fatalf("limits %v", f.limits)
	}
}
