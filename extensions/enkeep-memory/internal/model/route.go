package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Route is the chain of models a call goes through (CHANGELOG 0.2.0-beta.30): the primary models first, in order;
// when none of them answers, one fallback picked by how long the call is: Long for a call whose estimated size (the
// input and the room for the answer, Estimate) is above LongAbove, Short for the rest.
//
// A call names its model with model.complete's "models" field, which a hub adds on the extension's own connection
// (docs/model-routes.md). A hub without the field turns the call down as invalid params: the route then sends it
// the old way, on whatever model the extension's purpose has, and asks again after legacyRetry. A nil Route, or one
// with no models, is the old behaviour.
type Route struct {
	Primary   []string // tried first, in order
	Long      string   // the fallback of a long call
	Short     string   // the fallback of a short call
	LongAbove int      // estimated tokens; a call above it is long
	// PrimaryTimeout bounds a primary model's attempt, in seconds (0: the step's own limit), so that a slow primary
	// hands over to the fallback in time; the fallback keeps the step's limit.
	PrimaryTimeout int
	Stats          *Stats // nil: nothing counted
	Logf           func(format string, args ...any)
	Now            func() time.Time

	mu          sync.Mutex
	legacyUntil time.Time // the hub turned "models" down: calls go the old way until then
}

// legacyRetry is how long the route keeps sending calls the old way after a hub turned "models" down: a hub upgrade
// restarts the extension anyway, so this only bounds how long a hub that learns the field mid-run is not used.
const legacyRetry = time.Hour

// Failure reasons: why a model's attempt did not answer (each counted per model in Stats).
const (
	ReasonError       = "error"       // model_failed, a network error
	ReasonTimeout     = "timeout"     // the hub's timeout on the attempt
	ReasonEmpty       = "empty"       // an answer with no text
	ReasonFormat      = "format"      // the answer did not match the schema after the hub's second try (invalid_output)
	ReasonRefused     = "refused"     // the model declined the input
	ReasonUnavailable = "unavailable" // the hub cannot run this model now (model_paused: quota or load)
	ReasonNotOffered  = "not_offered" // the extension's connection has no such model (model_unavailable)
)

// Size classes of a call.
const (
	SizeLong  = "long"
	SizeShort = "short"
)

// Enabled says whether calls go through the chain.
func (rt *Route) Enabled() bool { return rt != nil && len(rt.models(SizeShort)) > 0 }

// Estimate is a call's size in tokens: the input (the system prompt, the user turn and the answer's schema, which the
// hub puts into the system prompt) and the room the call asks for its answer (MaxTokens; 4096, the hub's default,
// when unset). The input is counted as a quarter token per ASCII byte and one token per other character (Chinese
// text runs at about a token a character), so it errs on the long side for Chinese.
func Estimate(r Request) int {
	in := 0
	for _, s := range []string{r.System, r.User, string(r.Schema)} {
		ascii := 0
		for i := 0; i < len(s); {
			c, n := utf8.DecodeRuneInString(s[i:])
			if c < utf8.RuneSelf {
				ascii++
			} else {
				in++
			}
			i += n
		}
		in += (ascii + 3) / 4
	}
	out := r.MaxTokens
	if out <= 0 {
		out = 4096
	}
	return in + out
}

// Size is the class of a call of est tokens.
func (rt *Route) Size(est int) string {
	if rt.LongAbove > 0 && est > rt.LongAbove {
		return SizeLong
	}
	return SizeShort
}

// models is the chain of a call of the size: the primary models, then the size's fallback; each once.
func (rt *Route) models(size string) []string {
	var out []string
	add := func(m string) {
		m = strings.TrimSpace(m)
		for _, x := range out {
			if x == m {
				return
			}
		}
		if m != "" {
			out = append(out, m)
		}
	}
	for _, m := range rt.Primary {
		add(m)
	}
	if size == SizeLong {
		add(rt.Long)
	} else {
		add(rt.Short)
	}
	return out
}

// hasFallback says whether the size's fallback is a model of its own, after the primaries.
func (rt *Route) hasFallback(size string) bool {
	f := strings.TrimSpace(rt.Short)
	if size == SizeLong {
		f = strings.TrimSpace(rt.Long)
	}
	if f == "" {
		return false
	}
	for _, m := range rt.Primary {
		if strings.TrimSpace(m) == f {
			return false
		}
	}
	return true
}

func (rt *Route) now() time.Time {
	if rt.Now != nil {
		return rt.Now()
	}
	return time.Now()
}

func (rt *Route) legacy() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.now().Before(rt.legacyUntil)
}

func (rt *Route) setLegacy() {
	rt.mu.Lock()
	rt.legacyUntil = rt.now().Add(legacyRetry)
	rt.mu.Unlock()
}

func (rt *Route) logf(format string, args ...any) {
	if rt.Logf != nil {
		rt.Logf(format, args...)
	}
}

// errNoModels: the hub turned the call down because it does not know "models".
var errNoModels = errors.New("the hub does not take model.complete's models")

// attemptError is why one model's attempt did not answer.
type attemptError struct {
	reason string
	err    error     // what the caller gets when this is the last failure (ErrRefused, ErrUnavailable wrapped, …)
	reply  *Response // invalid_output: the reply the hub returned anyway
	fatal  bool      // nothing else is tried: the hub cannot run a model for the extension at all, or ctx ended
}

func (e *attemptError) Error() string { return e.err.Error() }

// Attempt is one model's try in a call, as Stats keeps it.
type Attempt struct {
	Model  string  `json:"model"`
	Reason string  `json:"reason,omitempty"` // empty: it answered
	Secs   float64 `json:"secs"`
}

// complete runs r through the chain; try makes one attempt on a model, legacy sends the call the old way.
func (rt *Route) complete(ctx context.Context, r Request, try func(context.Context, Request, string) (Response, error),
	legacy func(context.Context, Request) (Response, error)) (Response, error) {
	est := Estimate(r)
	size := rt.Size(est)
	if rt.legacy() {
		return rt.old(ctx, r, est, size, legacy, "hub without models")
	}
	chain := rt.models(size)
	began := rt.now()
	var attempts []Attempt
	var last *attemptError
	var kept *Response
	primaries := len(chain) - 1 // the last one is the size's fallback, unless it is one of the primaries
	if !rt.hasFallback(size) {
		primaries = len(chain)
	}
	for i, m := range chain {
		ri := r
		if i < primaries && rt.PrimaryTimeout > 0 {
			ri.Timeout = rt.PrimaryTimeout
		}
		start := rt.now()
		resp, err := try(ctx, ri, m)
		secs := rt.now().Sub(start).Seconds()
		if err == nil {
			attempts = append(attempts, Attempt{Model: m, Secs: secs})
			rt.done(began, r, est, size, attempts, m, i > 0, "")
			return resp, nil
		}
		if errors.Is(err, errNoModels) {
			rt.setLegacy()
			return rt.old(ctx, r, est, size, legacy, "hub without models")
		}
		var ae *attemptError
		if !errors.As(err, &ae) {
			ae = &attemptError{reason: ReasonError, err: err}
		}
		if ctx.Err() != nil {
			ae.fatal = true
		}
		attempts = append(attempts, Attempt{Model: m, Reason: ae.reason, Secs: secs})
		if ae.fatal {
			rt.done(began, r, est, size, attempts, "", false, ae.reason)
			return Response{}, ae.err
		}
		if ae.reply != nil {
			kept = ae.reply
		}
		last = ae
	}
	if allNotOffered(attempts) {
		// None of the route's models is on the extension's connection (another user's gateway, say): the route is
		// no use there, the purpose's own model is.
		return rt.old(ctx, r, est, size, legacy, "no route model on the connection")
	}
	if kept != nil {
		// No model matched the schema: as before the chain, the reply the hub returned anyway goes to the caller's
		// own checks of each item, rather than lose the whole unit.
		rt.done(began, r, est, size, attempts, kept.Model, kept.Model != attempts[0].Model, ReasonFormat)
		return *kept, nil
	}
	rt.done(began, r, est, size, attempts, "", false, last.reason)
	return Response{}, last.err
}

// old sends the call the old way, on the purpose's model, and counts it.
func (rt *Route) old(ctx context.Context, r Request, est int, size string, legacy func(context.Context, Request) (Response, error),
	why string) (Response, error) {
	start := rt.now()
	resp, err := legacy(ctx, r)
	a := Attempt{Model: resp.Model, Secs: rt.now().Sub(start).Seconds()}
	if a.Model == "" {
		a.Model = "(purpose)"
	}
	if err != nil {
		a.Reason = ReasonError
	}
	if rt.Stats != nil {
		c := Call{At: start.UTC().Format(time.RFC3339), Step: r.Step, Est: est, Size: size, Attempts: []Attempt{a}, Legacy: why}
		if err == nil {
			c.Answered = a.Model
		}
		rt.Stats.add(c)
	}
	rt.logf("enkeep-memory: model call %s: est %d tokens (%s), old way (%s): %s", r.Step, est, size, why, outcome(a))
	return resp, err
}

// done counts and logs a call that went through the chain.
func (rt *Route) done(began time.Time, r Request, est int, size string, attempts []Attempt, answered string, fellBack bool, failed string) {
	var tried []string
	for _, a := range attempts {
		tried = append(tried, outcome(a))
	}
	switch {
	case answered != "" && fellBack:
		rt.logf("enkeep-memory: model call %s: est %d tokens (%s): answered by %s after fallback; tried %s", r.Step, est,
			size, answered, strings.Join(tried, ", "))
	case answered != "":
		rt.logf("enkeep-memory: model call %s: est %d tokens (%s): answered by %s; tried %s", r.Step, est, size, answered,
			strings.Join(tried, ", "))
	default:
		rt.logf("enkeep-memory: model call %s: est %d tokens (%s): no model answered (%s); tried %s", r.Step, est, size,
			failed, strings.Join(tried, ", "))
	}
	if rt.Stats != nil {
		rt.Stats.add(Call{At: began.UTC().Format(time.RFC3339), Step: r.Step, Est: est, Size: size, Attempts: attempts,
			Answered: answered, FellBack: fellBack})
	}
}

func allNotOffered(attempts []Attempt) bool {
	for _, a := range attempts {
		if a.Reason != ReasonNotOffered {
			return false
		}
	}
	return len(attempts) > 0
}

func outcome(a Attempt) string {
	if a.Reason == "" {
		return fmt.Sprintf("%s ok %.0fs", a.Model, a.Secs)
	}
	return fmt.Sprintf("%s %s %.0fs", a.Model, a.Reason, a.Secs)
}

// Stats is what the route counted since Since, kept in a file of the extension's data directory
// (model-routes.json) so it outlives restarts: per model, the attempts, answers and failures by reason; per call,
// whether a fallback answered; the most recent calls.
type Stats struct {
	mu   sync.Mutex
	path string
	c    Counts
}

// Counts is what Stats counted: a plain value, safe to pass around.
type Counts struct {
	Since    string                 `json:"since"`
	Calls    int                    `json:"calls"`     // calls through the chain or the old way
	FellBack int                    `json:"fell_back"` // calls a fallback model answered
	Failed   int                    `json:"failed"`    // calls no model answered
	Legacy   int                    `json:"legacy"`    // calls sent the old way (a hub without models)
	Models   map[string]*ModelCount `json:"models"`
	Recent   []Call                 `json:"recent"` // the last recentCalls calls, oldest first
}

// ModelCount is one model's attempts.
type ModelCount struct {
	Attempts int            `json:"attempts"`
	Answered int            `json:"answered"`
	AsBackup int            `json:"as_backup"` // answers it gave as a fallback
	Failures map[string]int `json:"failures,omitempty"`
	Secs     float64        `json:"secs"` // the time its attempts took, together
}

// Call is one call as Stats keeps it.
type Call struct {
	At       string    `json:"at"`
	Step     string    `json:"step"`
	Est      int       `json:"est"`
	Size     string    `json:"size"`
	Attempts []Attempt `json:"attempts"`
	Answered string    `json:"answered,omitempty"`
	FellBack bool      `json:"fell_back,omitempty"`
	Legacy   string    `json:"legacy,omitempty"` // why it went the old way
}

const recentCalls = 50

// StatsFile is the name of the stats' file in the data directory.
const StatsFile = "model-routes.json"

// OpenStats reads the stats kept in dir (none yet: empty ones, from now).
func OpenStats(dir string, now time.Time) *Stats {
	s := &Stats{path: filepath.Join(dir, StatsFile)}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.c)
	}
	if s.c.Since == "" {
		s.c.Since = now.UTC().Format(time.RFC3339)
	}
	if s.c.Models == nil {
		s.c.Models = map[string]*ModelCount{}
	}
	return s
}

func (s *Stats) add(c Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := &s.c
	if n.Models == nil {
		n.Models = map[string]*ModelCount{}
	}
	n.Calls++
	switch {
	case c.Legacy != "":
		n.Legacy++
	case c.FellBack && c.Answered != "":
		n.FellBack++
	}
	if c.Answered == "" {
		n.Failed++
	}
	for i, a := range c.Attempts {
		m := n.Models[a.Model]
		if m == nil {
			m = &ModelCount{}
			n.Models[a.Model] = m
		}
		m.Attempts++
		m.Secs += a.Secs
		if a.Reason == "" {
			m.Answered++
			if i > 0 {
				m.AsBackup++
			}
		} else {
			if m.Failures == nil {
				m.Failures = map[string]int{}
			}
			m.Failures[a.Reason]++
		}
	}
	n.Recent = append(n.Recent, c)
	if len(n.Recent) > recentCalls {
		n.Recent = append([]Call(nil), n.Recent[len(n.Recent)-recentCalls:]...)
	}
	if s.path != "" {
		if b, err := json.MarshalIndent(n, "", " "); err == nil {
			_ = writeFile(s.path, b)
		}
		appendCall(filepath.Join(filepath.Dir(s.path), CallsFile), c)
	}
}

// CallsFile is the log of every call (one JSON line each: step, estimate, size, each model's attempt and why it
// failed, the model that answered; never what was sent or answered), beside the stats; at callsMost bytes it moves
// to CallsFile+".1", which keeps one older log.
const CallsFile = "model-calls.jsonl"

const callsMost = 2 << 20

func appendCall(path string, c Call) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > callsMost {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(c)
	_, _ = f.Write(append(b, '\n'))
}

// Snapshot is a copy of the counts, safe to read while calls go on.
func (s *Stats) Snapshot() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.c)
	var c Counts
	_ = json.Unmarshal(b, &c)
	return c
}

// Summary says the stats in a few lines: the calls, the fallback rate, and each model's attempts.
func (s Counts) Summary() string {
	var b strings.Builder
	rate := 0.0
	if s.Calls > 0 {
		rate = float64(s.FellBack) / float64(s.Calls) * 100
	}
	fmt.Fprintf(&b, "Model calls since %s: %d, answered by a fallback %d (%.1f%%), unanswered %d, sent the old way %d.",
		s.Since, s.Calls, s.FellBack, rate, s.Failed, s.Legacy)
	names := make([]string, 0, len(s.Models))
	for n := range s.Models {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m := s.Models[n]
		fmt.Fprintf(&b, "\n- %s: %d attempt(s), %d answered (%d as fallback)", n, m.Attempts, m.Answered, m.AsBackup)
		if len(m.Failures) > 0 {
			var rs []string
			for r := range m.Failures {
				rs = append(rs, r)
			}
			sort.Strings(rs)
			var parts []string
			for _, r := range rs {
				parts = append(parts, fmt.Sprintf("%s %d", r, m.Failures[r]))
			}
			fmt.Fprintf(&b, ", failed: %s", strings.Join(parts, ", "))
		}
		if m.Attempts > 0 {
			fmt.Fprintf(&b, ", %.0fs on average", m.Secs/float64(m.Attempts))
		}
	}
	return b.String()
}
