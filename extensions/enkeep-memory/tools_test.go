package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/curate"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/kbsrc"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/schema"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/store"
	"github.com/nwverse/persona-extensions/extensions/enkeep-memory/internal/stub"
)

func toolApp(t *testing.T) (*app, *store.Store) {
	t.Helper()
	fx, err := kbsrc.LoadFixture("testdata/sample")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := curate.Defaults()
	cfg.Mode, cfg.AdmitAllBelowTokens, cfg.BackfillDays = "shadow", 0, 0
	now := time.Date(2026, 11, 9, 4, 0, 0, 0, time.UTC)
	a := newApp()
	a.c = &curate.Curator{Src: fx, Model: &stub.Model{}, St: st, Cfg: cfg, Loc: time.UTC, Now: func() time.Time { return now }}
	return a, st
}

func call(t *testing.T, a *app, name string, args any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"contribution": connectorID, "name": name, "args": args})
	out, err := a.connector(t.Context(), "tools.call", raw)
	if err != nil {
		return "", err
	}
	return out.(map[string]any)["text"].(string), nil
}

// The tool lists two tools; the rerun tool queues the days once, refuses days not over and more than 31, and the
// status tool says what is done and what waits.
func TestRerunTool(t *testing.T) {
	a, st := toolApp(t)
	list, err := a.connector(t.Context(), "tools.list", nil)
	if err != nil || len(list.(map[string]any)["tools"].([]tool)) != 2 {
		t.Fatalf("tools.list: %v %v", list, err)
	}
	st.S.Reruns = []string{"2026-11-03@" + schema.DailyPrompt} // done with the current prompt already
	text, err := call(t, a, "enkeep_rerun", map[string]string{"from": "2026-11-02", "to": "2026-11-04"})
	if err != nil || !strings.Contains(text, "Queued 2 day(s)") || !strings.Contains(text, "skipped: 2026-11-03") {
		t.Fatalf("rerun: %q %v", text, err)
	}
	if !slices.Equal(st.S.RerunQueue, []string{"2026-11-02", "2026-11-04"}) {
		t.Fatalf("queue: %v", st.S.RerunQueue)
	}
	if text, _ := call(t, a, "enkeep_rerun", map[string]string{"from": "2026-11-02", "to": "2026-11-02"}); !strings.Contains(text, "Queued 0") {
		t.Fatalf("asked twice: %q", text)
	}
	for _, bad := range []map[string]string{
		{"from": "2026-11-08", "to": "2026-11-09"},           // today is not over
		{"from": "2026-09-01", "to": "2026-10-15"},           // more than 31 days
		{"from": "2026-11-04", "to": "2026-11-02"},           // backwards
		{"from": "yesterday", "to": "2026-11-02"},            // not a day
		{"from": "2026-11-01", "to": "2026-11-02", "x": "y"}, // unknown field
	} {
		if _, err := call(t, a, "enkeep_rerun", bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	status, err := call(t, a, "enkeep_rerun_status", map[string]any{})
	if err != nil || !strings.Contains(status, "0 day(s) done, 2 waiting") || !strings.Contains(status, "2026-11-02, 2026-11-04") {
		t.Fatalf("status: %q %v", status, err)
	}
}

// Days the tool queued run like rerun_range's, once each, and the status says they are done.
func TestRerunToolDaysRun(t *testing.T) {
	a, st := toolApp(t)
	if _, err := call(t, a, "enkeep_rerun", map[string]string{"from": "2026-11-03", "to": "2026-11-04"}); err != nil {
		t.Fatal(err)
	}
	settle := func() {
		for i := 0; i < 600; i++ {
			a.mu.Lock()
			idle := a.running == ""
			a.mu.Unlock()
			if idle {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("a run never ended")
	}
	for i := 0; i < 8; i++ {
		a.tick(t.Context())
		settle()
	}
	runs := 0
	for _, r := range st.S.Runs {
		if r.Kind == "daily" && (r.Period == "2026-11-03" || r.Period == "2026-11-04") {
			runs++
		}
	}
	if runs != 2 {
		t.Errorf("each queued day once, got %d runs", runs)
	}
	status, _ := call(t, a, "enkeep_rerun_status", map[string]any{})
	if !strings.Contains(status, "2 day(s) done, 0 waiting") {
		t.Errorf("status: %q", status)
	}
}
