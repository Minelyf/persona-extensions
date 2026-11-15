package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"
)

// The cold-session backup page: machine filter, cloud status, search over the local cold index
// (deep full-text search on demand), results with their location and restore. It is drawn in the
// main page's 会话与台账 tab and as its own nav page, Agent 对话.

const (
	sessionsUI      = "disklessly-sessions"
	allMachines     = "all"
	coldShown       = 50
	coldSearchHint  = "搜索会话：标题、首条消息、项目、关键词"
	coldOffloadText = "冷会话上云（读回校验后删本地）"
	coldRefreshText = "刷新云端索引与状态"
	coldDeepText    = "全文搜索"
	// coldQueryRunes keeps a query well under the 2000-character Search.value limit.
	coldQueryRunes = 200
)

// clipQuery trims a search query and cuts it to coldQueryRunes runes.
func clipQuery(q string) string {
	q = strings.TrimSpace(q)
	if r := []rune(q); len(r) > coldQueryRunes {
		q = strings.TrimSpace(string(r[:coldQueryRunes]))
	}
	return q
}

// sessPage is the Agent 对话 page state: the machine filter and the query only (16 KB limit).
type sessPage struct {
	Machine string `json:"m,omitempty"`
	Query   string `json:"q,omitempty"`
}

// deepResult carries a deep search's hits into one render; they are never stored in page state.
type deepResult struct {
	hits []ColdHit
	errs []string
}

func locationTone(loc string) string {
	switch loc {
	case LocCloud:
		return wire.TonePositive
	case LocMissing:
		return wire.ToneDanger
	case LocLocal:
		return wire.ToneNeutral
	}
	return wire.ToneCaution
}

// coldSections draws the cold backup status, actions and results for machine m (nil: all machines).
func (d draw) coldSections(m *Machine, query string, deep *deepResult) []wire.Section {
	query = clipQuery(query)
	machineArg := ""
	if m != nil {
		machineArg = m.ID
	}
	var machines []*Machine
	for _, mm := range d.st.Machines {
		if (m == nil && mm.Enabled) || (m != nil && mm.ID == m.ID) {
			machines = append(machines, mm)
		}
	}

	counts := map[string]int{}
	var total, indexed int
	var capped bool
	var lastCheck time.Time
	byState := map[string]int{}
	var reading []string
	type machineRow struct {
		m     *Machine
		cat   *ColdCatalog
		state string
	}
	var rows []machineRow
	for _, mm := range machines {
		cat := d.st.cold(mm.ID)
		state := cat.stateCode()
		byState[state]++
		rows = append(rows, machineRow{mm, cat, state})
		if j, ok := d.jobs[mm.ID]; ok && j.running() {
			reading = append(reading, machineLabel(mm))
		}
		if cat == nil {
			continue
		}
		total += cat.Total
		indexed += len(cat.Entries)
		capped = capped || cat.Capped
		if cat.Backend.CheckedAt.After(lastCheck) {
			lastCheck = cat.Backend.CheckedAt
		}
		for _, e := range cat.Entries {
			counts[cat.location(e, mm, d.now)]++
		}
	}
	slices.SortStableFunc(rows, func(a, b machineRow) int { return cmp.Compare(coldStateRank(a.state), coldStateRank(b.state)) })

	// The summary counts machines by state ("2 正常 · 2 未安装 CLI · 1 CLI 版本过旧"), worst first.
	var states []string
	for s := range byState {
		states = append(states, s)
	}
	slices.SortFunc(states, func(a, b string) int { return cmp.Compare(coldStateRank(a), coldStateRank(b)) })
	var parts []string
	worst := wire.TonePositive
	for _, s := range states {
		parts = append(parts, fmt.Sprintf("%d %s", byState[s], coldStateLabel(s)))
		if t := coldStateTone(s); t == wire.ToneDanger || (t == wire.ToneCaution && worst == wire.TonePositive) {
			worst = t
		}
	}
	machineStat := wire.Stat{Label: "机器", Value: fmt.Sprintf("%d 台", len(machines)), Detail: strings.Join(parts, " · "), Tone: worst}
	if len(reading) > 0 {
		machineStat.Detail = fmt.Sprintf("正在读取 %d 台 · %s", len(reading), machineStat.Detail)
	}
	checked := "从未"
	if !lastCheck.IsZero() {
		checked = d.when(lastCheck)
	}
	indexDetail := "最近检查 " + checked
	if capped {
		indexDetail = fmt.Sprintf("仅保留最新 %d 个（共 %d 个）· %s", indexed, total, indexDetail)
	}
	status := wire.Section{
		Title:  "云端会话备份",
		Footer: "位置只按最近 24 小时内的检查判断：后端不可达、未检查或机器离线时显示「未知」，从不当作已丢失。",
		Items: []wire.Component{{
			Type: wire.ComponentStatGrid,
			Stats: []wire.Stat{
				{Label: "已索引会话", Value: fmt.Sprintf("%d 个", indexed), Detail: indexDetail},
				{Label: LocCloud, Value: fmt.Sprintf("%d 个", counts[LocCloud]), Tone: toneIf(counts[LocCloud] > 0, wire.TonePositive)},
				{Label: "未知 / 云端缺失", Value: fmt.Sprintf("%d / %d", counts[LocUnknown], counts[LocMissing]),
					Tone: cmp.Or(toneIf(counts[LocMissing] > 0, wire.ToneDanger), toneIf(counts[LocUnknown] > 0, wire.ToneCaution))},
				machineStat,
			},
		}},
	}
	if len(reading) > 0 {
		status.Items = append(status.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
			Text: clip("正在后台读取："+strings.Join(reading, "、")+"。完成后页面自动更新。", 200)})
	}
	if m != nil {
		status.Items = append(status.Items, wire.Component{
			Type:   wire.ComponentButton,
			Title:  coldOffloadText,
			Action: &wire.Action{ID: "cold-offload", Values: map[string]string{"m": m.ID}, Confirm: sessionOffloadConfirm(m)},
		})
	}
	status.Items = append(status.Items, wire.Component{
		Type: wire.ComponentButton, Title: coldRefreshText,
		Action: &wire.Action{ID: "cold-refresh", Values: map[string]string{"m": machineArg}},
	})
	if strings.TrimSpace(query) != "" {
		status.Items = append(status.Items, wire.Component{
			Type: wire.ComponentButton, Title: coldDeepText,
			Action: &wire.Action{ID: "cold-deep", Values: map[string]string{"m": machineArg, "q": query}},
		})
	}

	machineSec := wire.Section{Key: "cold-machines", Title: "各机器状态"}
	for _, r := range rows {
		machineSec.Items = append(machineSec.Items, d.machineStateItems(r.m, r.cat, r.state)...)
	}

	var hits []ColdHit
	title := "最近归档的会话"
	if deep != nil {
		hits = deep.hits
		title = fmt.Sprintf("全文搜索「%s」（%d 个）", clip(query, 24), len(hits))
	} else {
		hits = SearchCold(d.st, query, machineArg, 0, d.now)
		if strings.TrimSpace(query) != "" {
			title = fmt.Sprintf("搜索「%s」（%d 个）", clip(query, 24), len(hits))
		}
	}
	results := wire.Section{Title: title}
	if deep != nil {
		for _, e := range deep.errs[:min(len(deep.errs), 4)] {
			results.Items = append(results.Items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary, Text: clip("全文搜索未完成："+e, 200)})
		}
	}
	shown := hits[:min(len(hits), coldShown)]
	for _, h := range shown {
		results.Items = append(results.Items, d.coldRow(h))
	}
	var notes []string
	if len(hits) > len(shown) {
		notes = append(notes, fmt.Sprintf("显示最新 %d 个，共 %d 个；输入更多关键词缩小范围。", len(shown), len(hits)))
	}
	if capped {
		notes = append(notes, fmt.Sprintf("本地索引每台机器只保留最新 %d 个会话，更早的请用「全文搜索」。", maxColdEntries))
	}
	results.Footer = strings.Join(notes, " ")
	if len(shown) == 0 {
		detail := "扫描机器或点击「" + coldRefreshText + "」读取 disklessly 的会话索引。"
		if strings.TrimSpace(query) != "" {
			detail = "本地索引没有匹配的会话；可试试「" + coldDeepText + "」在机器上搜索完整摘要。"
		}
		results.Items = append(results.Items, wire.Component{Type: wire.ComponentEmpty, Icon: "chat", Title: "没有会话", Detail: detail})
	}
	if len(machineSec.Items) == 0 {
		return []wire.Section{status, results}
	}
	return []wire.Section{status, machineSec, results}
}

// refreshAfter asks the app to draw the page again while a background job runs (hubs that do
// not take ui.invalidate still redraw); 0 otherwise.
func (d draw) refreshAfter() int {
	for _, j := range d.jobs {
		if j.running() {
			return coldPollSeconds
		}
	}
	return 0
}

// coldPollSeconds is the redraw interval while a job runs (the view schema's minimum).
const coldPollSeconds = 30

// machineStateItems draws one machine's row (name, state, cause) and the actions that fix it.
func (d draw) machineStateItems(m *Machine, cat *ColdCatalog, state string) []wire.Component {
	name := machineLabel(m)
	row := wire.Component{
		Type:  wire.ComponentRow,
		Key:   "mach-" + m.ID,
		Icon:  "gauge",
		Title: name,
		Badge: &wire.Badge{Text: coldStateLabel(state), Tone: coldStateTone(state)},
	}
	var sub []string
	if p := cmp.Or(catPlatform(cat), m.Platform); p != "" {
		sub = append(sub, p)
	}
	if cat != nil && cat.CLI.Version != "" {
		sub = append(sub, "disklessly "+shortVersion(cat.CLI.Version))
	}
	if cat != nil && !cat.CheckedAt.IsZero() {
		sub = append(sub, d.when(cat.CheckedAt)+"检查")
	} else if cat != nil && !cat.RefreshedAt.IsZero() {
		sub = append(sub, d.when(cat.RefreshedAt)+"检查")
	}
	row.Subtitle = strings.Join(sub, " · ")

	errText := ""
	if cat != nil {
		errText = cat.Error
	}
	refresh := func(title string) wire.Component {
		return wire.Component{Type: wire.ComponentButton, Title: title + " " + name,
			Action: &wire.Action{ID: "cold-refresh", Values: map[string]string{"m": m.ID}}}
	}
	var actions []wire.Component
	switch state {
	case ColdReady:
		n, cloud, missing := 0, 0, 0
		for _, e := range cat.Entries {
			n++
			switch cat.location(e, m, d.now) {
			case LocCloud:
				cloud++
			case LocMissing:
				missing++
			}
		}
		b := cat.Backend
		row.Detail = fmt.Sprintf("已索引 %d 个 · 在云端 %d 个 · 云端缺失 %d 个 · 备份后端 %s 可达（%d ms）",
			n, cloud, missing, cmp.Or(b.ID, b.Type, "backend"), b.LatencyMs)
		if len(cat.Missing) > 0 {
			row.Detail += fmt.Sprintf("；备份后端缺少 %d 个归档分段，相关会话无法恢复", len(cat.Missing))
		}
		if len(cat.Unchecked) > 0 || cat.TimedOut {
			row.Detail += fmt.Sprintf("；%d 个归档分段未能确认", len(cat.Unchecked))
		}
		actions = append(actions, refresh("刷新"))
	case ColdNoCLI, ColdCLIOld:
		if state == ColdNoCLI {
			row.Detail = "这台电脑上没有 disklessly CLI，会话备份在这里没有开启。"
		} else {
			row.Detail = cliOldMessage(cat, m) + "。"
		}
		offer := installSourceFor(d.st, m)
		if offer.src != nil {
			row.Detail += fmt.Sprintf("可以从 %s 复制安装 disklessly %s。", machineLabel(offer.src), shortVersion(offer.cli.Version))
			actions = append(actions, wire.Component{Type: wire.ComponentButton, Title: "从 " + machineLabel(offer.src) + " 复制安装",
				Action: &wire.Action{ID: "cold-install", Values: map[string]string{"m": m.ID, "id": offer.src.ID}, Confirm: installConfirm(offer, m)}})
		} else {
			row.Detail += offer.reason + "。"
		}
		actions = append(actions, wire.Component{Type: wire.ComponentLink, Title: "如何安装 disklessly CLI", URL: coldInstallDocsURL})
		actions = append(actions, refresh("重新检查"))
	case ColdNoBackend:
		row.Detail = "这台电脑上的 disklessly 还没有设置备份后端，冷会话不会上传。在这台电脑上为 disklessly 设置备份后端后点「重新检查」。"
		if cat.Detail != "" {
			row.Detail += "disklessly 的提示：" + cat.Detail
		}
		actions = append(actions, refresh("重新检查"))
	case ColdBackendDown:
		b := cat.Backend
		row.Detail = fmt.Sprintf("备份后端 %s 连不上：%s（%d ms），云端位置显示为未知。", cmp.Or(b.ID, b.Type, "backend"), cmpOr(b.Error, "backend unreachable"), b.LatencyMs)
		actions = append(actions, refresh("重新检查"))
	case ColdOffline:
		row.Detail = sshCauseText(cat.Cause) + "：" + errText
		actions = append(actions, refresh("重试"))
	case ColdUnreadable:
		row.Detail = cmpOr(errText, "disklessly 的回答无法识别")
		if cat.Detail != "" {
			row.Detail += "。它输出的第一行：" + cat.Detail
		}
		actions = append(actions, refresh("重试"))
	default:
		row.Detail = "还没有读取过这台电脑的会话索引。"
		actions = append(actions, refresh("读取"))
	}
	row.Detail = clip(row.Detail, 400)

	items := []wire.Component{row}
	if j, ok := d.jobs[m.ID]; ok {
		if j.running() {
			row.Badge = &wire.Badge{Text: "进行中", Tone: wire.ToneNeutral}
			items[0] = row
			items = append(items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
				Text: fmt.Sprintf("%s…（%s开始）", j.Label, d.when(j.Started))})
			return items // no actions while it runs: a second start would be refused anyway
		}
		if j.Kind != jobRefresh {
			text := j.Note
			if j.Err != "" {
				text = "失败：" + j.Err
			}
			if text != "" {
				items = append(items, wire.Component{Type: wire.ComponentText, Style: wire.TextStyleSecondary,
					Text: clip(fmt.Sprintf("%s · %s", d.when(j.Finished), text), 300)})
			}
		}
	}
	return append(items, actions...)
}

func catPlatform(c *ColdCatalog) string {
	if c == nil {
		return ""
	}
	return c.Platform
}

func (d draw) coldRow(h ColdHit) wire.Component {
	sub := []string{h.MachineName, cmp.Or(toolLabel(h.Tool), "Agent")}
	if p := cmp.Or(h.Project, lastPathElem(h.Cwd)); p != "" {
		sub = append(sub, p)
	}
	if t := entryTime(h.ColdEntry); !t.IsZero() {
		sub = append(sub, t.In(d.loc).Format(time.DateOnly))
	}
	if !h.Local {
		sub = append(sub, "来自其他设备")
	}
	row := wire.Component{
		Type:     wire.ComponentRow,
		Key:      "cold-" + h.MachineID + "-" + h.ID,
		Icon:     "chat",
		Title:    clip(cmp.Or(h.Title, h.FirstPrompt, h.SessionID, h.ID), 120),
		Subtitle: strings.Join(sub, " · "),
		Detail:   clip(cmp.Or(h.Preview, h.FirstPrompt), 240),
		Badge:    &wire.Badge{Text: h.Location, Tone: locationTone(h.Location)},
	}
	// Another device's row is not restored here: its original path belongs to that device.
	if h.Location != LocLocal && h.Local {
		confirm := fmt.Sprintf("从云端恢复到原路径 %s？", cmp.Or(h.Path, "（原路径）"))
		if hint := resumeHint(h.ColdEntry); hint != "" {
			confirm += "恢复后可运行 " + hint
		}
		row.Actions = []wire.RowAction{{Title: "恢复", Swipe: true, Action: wire.Action{
			ID:      "cold-restore",
			Values:  map[string]string{"m": h.MachineID, "id": h.ID},
			Confirm: confirm,
		}}}
	}
	return row
}

func lastPathElem(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// coldUIBudget bounds a deep search run inside a UI request (the hub allows about 12 seconds).
const coldUIBudget = 8 * time.Second

// coldDeep runs a deep search on the machines in parallel, within coldUIBudget and without
// holding the store lock while they answer.
func (a *app) coldDeep(ctx context.Context, machine, query string) (*deepResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, errors.New("请输入要全文搜索的内容")
	}
	if machine == allMachines {
		machine = ""
	}
	runner := a.run()
	var ms []*Machine
	if err := a.db().read(func(st *State) {
		for _, m := range deepTargets(st, machine) {
			mc := *m
			ms = append(ms, &mc)
		}
	}); err != nil {
		return nil, err
	}
	var runs map[string]deepRun
	if words := deepSearchWords(query); len(words) > 0 {
		runs = deepSearchMachines(ctx, runner, ms, words, coldShown, coldUIBudget)
	}
	var res deepResult
	err := a.db().read(func(st *State) {
		res.hits, res.errs = mergeDeepSearch(st, query, machine, coldShown, a.now(), runs)
	})
	return &res, err
}

// ---- Agent 对话 nav page ----

func (a *app) renderSessions(_ context.Context, r *ext.UIRequest) (*wire.View, error) {
	var p sessPage
	if err := r.DecodeState(&p); err != nil {
		return nil, &ext.Error{Code: "invalid", Message: "invalid page state"}
	}
	return a.sessionsView(r, p, nil)
}

func (a *app) sessionsView(r *ext.UIRequest, p sessPage, deep *deepResult) (*wire.View, error) {
	var v *wire.View
	err := a.db().read(func(st *State) { v = a.drawer(st, r).sessionsPage(p, deep) })
	return v, err
}

func (d draw) sessionsPage(p sessPage, deep *deepResult) *wire.View {
	var m *Machine
	if p.Machine != "" && p.Machine != allMachines {
		if m = d.st.machine(p.Machine); m == nil || !m.Enabled {
			m, p.Machine = nil, ""
		}
	}
	choices := []wire.Choice{{Value: allMachines, Title: "全部机器"}}
	for _, mm := range d.st.Machines {
		if mm.Enabled {
			choices = append(choices, wire.Choice{Value: mm.ID, Title: clip(machineLabel(mm), 28)})
		}
	}
	value := allMachines
	if m != nil {
		value = m.ID
	}
	return &wire.View{
		Title:        "Agent 对话",
		State:        ext.EncodeState(p),
		RefreshAfter: d.refreshAfter(),
		Search:       &wire.Search{Placeholder: coldSearchHint, Value: clipQuery(p.Query), Action: wire.Action{ID: "search"}},
		Filter:       &wire.Filter{Name: "machine", Title: "机器", Choices: choices, Value: value, Action: wire.Action{ID: "machine"}},
		Sections:     d.coldSections(m, p.Query, deep),
	}
}

func (a *app) actionSessions(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	var p sessPage
	if err := r.DecodeState(&p); err != nil {
		return nil, &ext.Error{Code: "invalid", Message: "invalid page state"}
	}
	res := &wire.ActionResult{}
	var deep *deepResult
	switch r.Action {
	case "machine":
		p.Machine = r.Values["machine"]
	case "search":
		p.Query = clipQuery(r.Values["q"])
	case "cold-deep":
		p.Query = clipQuery(cmp.Or(strings.TrimSpace(r.Values["q"]), p.Query))
		var err error
		if deep, err = a.coldDeep(ctx, cmp.Or(r.Values["m"], p.Machine), p.Query); err != nil {
			return nil, &ext.Error{Code: "invalid", Message: err.Error()}
		}
	default:
		toast, err := a.coldAction(ctx, r.Action, cmp.Or(r.Values["m"], p.Machine), r.Values["id"])
		if err != nil {
			return nil, &ext.Error{Code: "invalid", Message: err.Error()}
		}
		res.Toast = toast
	}
	var err error
	res.View, err = a.sessionsView(r, p, deep)
	return res, err
}
