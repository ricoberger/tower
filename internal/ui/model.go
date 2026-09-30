package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/snapshot"
)

// EditorStopDelay is how long a terminated editor may take to exit on
// shutdown before it is killed.
const EditorStopDelay = 5 * time.Second

// HintTimeout is how long a footer hint is shown before the footer returns
// to the key hints. Confirmations stay until they are answered.
const HintTimeout = 5 * time.Second

// DoneWindow is how recently a done item must have been updated to be shown
// in the Done group.
const DoneWindow = 7 * 24 * time.Hour

// Engine is the request API of the running engine. Requests are validated
// and applied by the engine.
type Engine interface {
	ManualRun(ctx context.Context, itemID string) error
	Dismiss(ctx context.Context, itemID string) error
	// PollNow starts a poll round of all sources unless one is running.
	PollNow(ctx context.Context) error
}

// Store is the part of the item store the UI uses: safe path accessors,
// artifact reads and action recording.
type Store interface {
	RunFiles
	AlertFiles
	ItemPath(id string) (string, error)
	RunPath(id string, n int) (string, error)
	HasRunFile(id string, n int, name string) (bool, error)
	AppendAction(id string, action item.Action, run int, now time.Time) (*item.Item, error)
}

// ResumeRequest identifies the session a resume hands over: the item's
// current run and its session at the time the user asked.
type ResumeRequest struct {
	ItemID    string
	Run       int
	SessionID string
	Placement string
}

// Options configures the model.
type Options struct {
	Feed   *snapshot.Feed
	Engine Engine
	Store  Store
	Now    func() time.Time
	// SeverityOrder orders items within a group; unknown severities sort
	// last.
	SeverityOrder []string
	// PrepareAfter is alerts.prepare_after (incoming countdown).
	PrepareAfter time.Duration
	// Editor is the editor executable.
	Editor string
	// Placement is ghostty.placement (used by c).
	Placement string
	// Resume launches the Ghostty handoff for the request and records the
	// resumed-session action once it succeeded.
	Resume func(ctx context.Context, r ResumeRequest) error
	// Open opens a URL in the browser.
	Open func(ctx context.Context, url string) error
	// Hint turns request errors into footer hints (nil: the error text).
	Hint func(error) string
	// Heartbeat is the display refresh interval (0: one second, negative:
	// disabled, for tests that send HeartbeatMsg themselves).
	Heartbeat time.Duration
	// EditorStopDelay overrides EditorStopDelay (tests).
	EditorStopDelay time.Duration
	// RenderMarkdown renders a Markdown document for a width in the
	// background (nil: RenderMarkdown; tests).
	RenderMarkdown func(src string, width int) []string
}

type focusArea int

const (
	focusList focusArea = iota
	focusPreview
)

type confirmKind int

const (
	confirmDismiss confirmKind = iota + 1
	confirmResume
)

// confirmation is a pending footer prompt bound to an item (and, for
// resume, a run and session).
type confirmation struct {
	kind      confirmKind
	id        string
	title     string
	run       int
	session   string
	placement string
}

// group is one displayed state group.
type group struct {
	label string
	items []snapshot.ItemView
}

// HeartbeatMsg drives the one-second display refresh.
type HeartbeatMsg time.Time

// ExecMsg asks the model to hand the terminal to an external command (the
// editor) and to deliver After's message once it exits.
type ExecMsg struct {
	Cmd   *EditorCommand
	After func(error) tea.Msg
}

// hintMsg shows a footer message.
type hintMsg struct {
	text string
	err  bool
}

type artifactsMsg artifacts

type progressMsg progress

type alertMsg alertDoc

// editorDoneMsg reports an editor exit; record is set for report opens.
type editorDoneMsg struct {
	what   string
	record bool
	id     string
	run    int
	err    error
}

// Model is the root Bubble Tea model.
type Model struct {
	ctx  context.Context
	opts Options

	width, height int
	now           time.Time

	snap    snapshot.Snapshot
	hasSnap bool

	groups  []group
	visible []snapshot.ItemView
	list    List
	// selected is the identity of the selected item ("" when none).
	selected string

	focus    focusArea
	showDone bool
	help     bool
	confirm  *confirmation

	footer    string
	footerErr bool
	// footerAt is when the footer hint was set.
	footerAt time.Time

	scroll int
	// helpScroll is the scroll offset of the help overlay.
	helpScroll int
	// art are the loaded artifacts of the selected finished run;
	// artLoading is the key of the load in flight.
	art        artifacts
	artLoading *previewKey
	// prog is the progress line of the selected executing run;
	// progLoading is set while a progress read is in flight.
	prog        progress
	progLoading *previewKey
	// alert is the alert.md of the selected item without a run;
	// alertLoading is set while an alert.md read is in flight.
	alert        alertDoc
	alertLoading bool
	// md is the latest rendered Markdown document of the preview;
	// mdPending is set while a render is in flight. mdPlain holds the plain
	// text shown until the rendering is available.
	md        mdCache
	mdPending bool
	mdPlain   mdCache
}

// New returns the model. ctx bounds all background requests.
func New(ctx context.Context, opts Options) *Model {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Heartbeat == 0 {
		opts.Heartbeat = time.Second
	}
	if opts.EditorStopDelay <= 0 {
		opts.EditorStopDelay = EditorStopDelay
	}
	if opts.RenderMarkdown == nil {
		opts.RenderMarkdown = RenderMarkdown
	}
	m := &Model{ctx: ctx, opts: opts, now: opts.Now()}
	if s, ok := opts.Feed.Latest(); ok {
		m.applySnapshot(s)
	}
	return m
}

// Init starts the snapshot subscription and the heartbeat.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(waitFeed(m.ctx, m.opts.Feed), m.tick(), m.loadPreview())
}

func (m *Model) tick() tea.Cmd {
	if m.opts.Heartbeat < 0 {
		return nil
	}
	return tea.Tick(m.opts.Heartbeat, func(t time.Time) tea.Msg { return HeartbeatMsg(t) })
}

// SelectedID returns the selected item ID ("" when nothing is selected).
func (m *Model) SelectedID() string { return m.selected }

// Footer returns the current footer message.
func (m *Model) Footer() string { return m.footer }

// Update handles a message and starts rendering the preview's Markdown
// document when it changed.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := m.update(msg)
	return m, tea.Batch(cmd, m.renderMarkdown())
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case markdownMsg:
		m.applyMarkdown(msg)
		return m, nil
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case SnapshotMsg:
		m.applySnapshot(snapshot.Snapshot(msg))
		return m, tea.Batch(waitFeed(m.ctx, m.opts.Feed), m.loadPreview())
	case HeartbeatMsg:
		m.now = m.opts.Now()
		if m.footer != "" && m.now.Sub(m.footerAt) >= HintTimeout {
			m.setHint("", false)
		}
		m.rebuild()
		return m, tea.Batch(m.tick(), m.loadPreview())
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case ExecMsg:
		after := msg.After
		return m, tea.Exec(msg.Cmd, func(err error) tea.Msg { return after(err) })
	case editorDoneMsg:
		return m, m.editorDone(msg)
	case hintMsg:
		m.setHint(msg.text, msg.err)
		return m, nil
	case artifactsMsg:
		if m.artLoading != nil && *m.artLoading == msg.key {
			m.artLoading = nil
		}
		if key, ok := m.wantKey(); ok && key == msg.key {
			m.art = artifacts(msg)
		}
		return m, m.loadPreview()
	case progressMsg:
		if m.progLoading != nil && m.progLoading.id == msg.id && m.progLoading.run == msg.run {
			m.progLoading = nil
		}
		if key, ok := m.wantKey(); ok && key.id == msg.id && key.run == msg.run {
			m.prog = progress(msg)
		}
		return m, nil
	case alertMsg:
		m.alertLoading = false
		id, ok := m.wantAlert()
		if !ok {
			return m, nil
		}
		if id != msg.id {
			// The selection changed while the read was in flight.
			return m, m.loadPreview()
		}
		m.alert = alertDoc(msg)
		return m, nil
	}
	return m, nil
}

// setHint shows a footer hint; it is cleared on the first heartbeat
// HintTimeout after it was set.
func (m *Model) setHint(text string, isErr bool) {
	m.footer, m.footerErr = text, isErr
	m.footerAt = m.opts.Now()
}

func (m *Model) hintErr(err error) tea.Msg {
	text := err.Error()
	if m.opts.Hint != nil {
		text = m.opts.Hint(err)
	}
	return hintMsg{text: text, err: true}
}

// applySnapshot replaces the engine state and rebuilds the list.
func (m *Model) applySnapshot(s snapshot.Snapshot) {
	m.snap, m.hasSnap = s, true
	m.now = m.opts.Now()
	m.rebuild()
}

// groupOf maps a state to its group index (-1: not displayed).
func groupOf(s item.State) int {
	switch s {
	case item.StateNeedsYou:
		return 0
	case item.StateQueued, item.StatePreparing:
		return 1
	case item.StateNew:
		return 2
	case item.StateSnoozed:
		return 3
	case item.StateResolved:
		return 4
	case item.StateDone:
		return 5
	}
	return -1
}

var groupLabels = []string{"Needs you", "Preparing", "Incoming", "Snoozed", "Resolved", "Done"}

func (m *Model) severityRank(s string) int {
	if i := slices.Index(m.opts.SeverityOrder, s); i >= 0 {
		return i
	}
	return len(m.opts.SeverityOrder)
}

// compareItems orders by severity (unknown last), then alert start (oldest
// first), then ID.
func (m *Model) compareItems(a, b snapshot.ItemView) int {
	if c := m.severityRank(a.Item.Severity) - m.severityRank(b.Item.Severity); c != 0 {
		return c
	}
	if c := a.Item.Alert.StartsAt.Compare(b.Item.Alert.StartsAt); c != 0 {
		return c
	}
	return strings.Compare(a.Item.ID, b.Item.ID)
}

// rebuild recomputes the groups and keeps the selection on the same item
// when it is still visible.
func (m *Model) rebuild() {
	buckets := make([][]snapshot.ItemView, len(groupLabels))
	cutoff := m.now.Add(-DoneWindow)
	for _, v := range m.snap.Items {
		if v.Item == nil {
			continue
		}
		g := groupOf(v.Item.State)
		if g < 0 {
			continue
		}
		if v.Item.State == item.StateDone && (!m.showDone || v.Item.UpdatedAt.Before(cutoff)) {
			continue
		}
		buckets[g] = append(buckets[g], v)
	}
	m.groups = m.groups[:0]
	m.visible = m.visible[:0]
	for i, b := range buckets {
		if len(b) == 0 {
			continue
		}
		slices.SortFunc(b, m.compareItems)
		m.groups = append(m.groups, group{label: groupLabels[i], items: b})
		m.visible = append(m.visible, b...)
	}
	prev := m.selected
	idx := slices.IndexFunc(m.visible, func(v snapshot.ItemView) bool { return v.Item.ID == m.selected })
	if idx >= 0 {
		m.list.Selected = idx
	} else {
		m.list.Selected = m.list.Clamp(len(m.visible))
	}
	m.syncSelected()
	if m.selected != prev {
		m.scroll = 0
	}
}

func (m *Model) syncSelected() {
	if len(m.visible) == 0 {
		m.selected = ""
		m.list.Selected = 0
		return
	}
	m.list.Selected = m.list.Clamp(len(m.visible))
	m.selected = m.visible[m.list.Selected].Item.ID
}

// selectedView returns the selected item.
func (m *Model) selectedView() (snapshot.ItemView, bool) {
	if m.selected == "" {
		return snapshot.ItemView{}, false
	}
	for _, v := range m.visible {
		if v.Item.ID == m.selected {
			return v, true
		}
	}
	return snapshot.ItemView{}, false
}

// findView returns an item of the latest snapshot by ID.
func (m *Model) findView(id string) (snapshot.ItemView, bool) {
	for _, v := range m.snap.Items {
		if v.Item != nil && v.Item.ID == id {
			return v, true
		}
	}
	return snapshot.ItemView{}, false
}

// wantKey returns the artifacts the preview of the selection needs.
func (m *Model) wantKey() (previewKey, bool) {
	v, ok := m.selectedView()
	if !ok {
		return previewKey{}, false
	}
	r, ok := v.Latest()
	if !ok {
		return previewKey{}, false
	}
	return previewKey{id: v.Item.ID, run: r.Number, outcome: r.Outcome}, true
}

// wantAlert returns the item whose alert.md the preview shows: the
// selected item when it has no run.
func (m *Model) wantAlert() (string, bool) {
	v, ok := m.selectedView()
	if !ok {
		return "", false
	}
	if _, hasRun := v.Latest(); hasRun {
		return "", false
	}
	return v.Item.ID, true
}

// loadPreview starts the artifact, progress or alert.md read the selection
// needs, off the update loop. At most one read of each kind is in flight.
func (m *Model) loadPreview() tea.Cmd {
	if m.opts.Store == nil {
		return nil
	}
	files := m.opts.Store
	if id, ok := m.wantAlert(); ok {
		if m.alertLoading {
			return nil
		}
		m.alertLoading = true
		prev := m.alert
		return func() tea.Msg { return alertMsg(loadAlert(files, prev, id)) }
	}
	key, ok := m.wantKey()
	if !ok {
		return nil
	}
	if key.outcome == item.OutcomeRunning {
		if m.progLoading != nil {
			return nil
		}
		k := key
		m.progLoading = &k
		prev := m.prog
		return func() tea.Msg { return progressMsg(loadProgress(files, prev, key.id, key.run)) }
	}
	if m.art.key == key || (m.artLoading != nil && *m.artLoading == key) {
		return nil
	}
	k := key
	m.artLoading = &k
	return func() tea.Msg { return artifactsMsg(loadArtifacts(files, key)) }
}

// handleKey dispatches a key press. Help and confirmations capture all keys
// so no item action can fire behind them.
func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	if m.confirm != nil {
		return m, m.handleConfirm(k)
	}
	if m.help {
		switch k {
		case "?", "esc":
			m.help = false
		case "q":
			return m, tea.Quit
		case "j", "down":
			m.helpScroll++
		case "k", "up":
			m.helpScroll = max(m.helpScroll-1, 0)
		case "g":
			m.helpScroll = 0
		case "G":
			m.helpScroll = 1 << 30 // clamped when rendered
		}
		return m, nil
	}
	switch k {
	case "q":
		return m, tea.Quit
	case "?":
		m.help = true
		m.helpScroll = 0
		return m, nil
	case "esc":
		m.setHint("", false)
		return m, nil
	case "tab":
		if m.focus == focusList {
			m.focus = focusPreview
		} else {
			m.focus = focusList
		}
		return m, nil
	case "d":
		m.showDone = !m.showDone
		m.rebuild()
		if m.showDone {
			m.setHint("showing done items updated in the last 7 days", false)
		} else {
			m.setHint("done items hidden", false)
		}
		return m, m.loadPreview()
	case "r":
		m.setHint("polling sources…", false)
		return m, m.request(func(ctx context.Context) (string, error) {
			return "poll started", m.opts.Engine.PollNow(ctx)
		})
	}
	if m.focus == focusPreview {
		switch k {
		case "j", "down":
			m.scroll++
			return m, nil
		case "k", "up":
			m.scroll = max(m.scroll-1, 0)
			return m, nil
		}
	}
	// g and G select the first and last item regardless of focus.
	if m.list.Handle(k, len(m.visible)) {
		prev := m.selected
		m.syncSelected()
		if m.selected != prev {
			m.scroll = 0
		}
		return m, m.loadPreview()
	}
	return m, m.itemAction(k)
}

// request runs an engine request in the background and reports its outcome
// in the footer.
func (m *Model) request(fn func(ctx context.Context) (string, error)) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		ok, err := fn(ctx)
		if err != nil {
			return m.hintErr(err)
		}
		return hintMsg{text: ok}
	}
}

// itemAction handles the keys acting on the selected item.
func (m *Model) itemAction(k string) tea.Cmd {
	switch k {
	case "enter", "o", "c", "C", "p", "x", "l", "b", "a":
	default:
		return nil
	}
	v, ok := m.selectedView()
	if !ok {
		m.setHint("no item selected", true)
		return nil
	}
	id := v.Item.ID
	switch k {
	case "p":
		m.setHint("requesting a preparation run…", false)
		return m.request(func(ctx context.Context) (string, error) {
			return "preparation run requested", m.opts.Engine.ManualRun(ctx, id)
		})
	case "x":
		m.confirm = &confirmation{kind: confirmDismiss, id: id, title: v.Item.Title}
		return nil
	case "b":
		return m.openURL(v)
	case "a":
		return m.openItemDir(id)
	case "c", "C":
		placement := m.opts.Placement
		if k == "C" {
			placement = "tab"
		}
		return m.resume(v, placement)
	}
	r, ok := v.Latest()
	if !ok {
		m.setHint("the item has no run yet", true)
		return nil
	}
	switch k {
	case "enter", "o":
		return m.openReport(id, r.Number)
	case "l":
		return m.openLogs(id, r.Number)
	}
	return nil
}

// handleConfirm handles a key while a confirmation is pending: only y
// confirms; n and esc cancel; q quits; everything else is ignored.
func (m *Model) handleConfirm(k string) tea.Cmd {
	c := m.confirm
	switch k {
	case "y":
		m.confirm = nil
		switch c.kind {
		case confirmDismiss:
			m.setHint("dismissing…", false)
			return m.request(func(ctx context.Context) (string, error) {
				return "dismissed", m.opts.Engine.Dismiss(ctx, c.id)
			})
		case confirmResume:
			// The confirmation is bound to the run and session it was
			// asked for; it never transfers to a newer run.
			v, ok := m.findView(c.id)
			r, has := v.Latest()
			if !ok || !has || r.Number != c.run || r.SessionID != c.session {
				m.setHint("the item's run changed; resume cancelled", true)
				return nil
			}
			return m.launchResume(c.id, c.run, c.session, c.placement)
		}
	case "n", "esc":
		m.confirm = nil
		m.setHint("cancelled", false)
	case "q":
		return tea.Quit
	}
	return nil
}

// Resumable reports whether a run has a session that actually started: a
// preassigned session ID of a run that failed before Copilot started is not
// resumable.
func Resumable(r item.Run) error {
	if r.SessionID == "" {
		return fmt.Errorf("run %d has no session", r.Number)
	}
	if r.StartedAt == nil {
		return fmt.Errorf("run %d has no session: it failed before Copilot started", r.Number)
	}
	return nil
}

func (m *Model) resume(v snapshot.ItemView, placement string) tea.Cmd {
	r, ok := v.Latest()
	if !ok {
		m.setHint("the item has no run to resume", true)
		return nil
	}
	if err := Resumable(r); err != nil {
		m.setHint(err.Error(), true)
		return nil
	}
	if r.Outcome == item.OutcomeRunning {
		m.confirm = &confirmation{
			kind: confirmResume, id: v.Item.ID, title: v.Item.Title,
			run: r.Number, session: r.SessionID, placement: placement,
		}
		return nil
	}
	return m.launchResume(v.Item.ID, r.Number, r.SessionID, placement)
}

func (m *Model) launchResume(id string, run int, session, placement string) tea.Cmd {
	if m.opts.Resume == nil {
		m.setHint("resume is not available", true)
		return nil
	}
	m.setHint("opening Ghostty…", false)
	req := ResumeRequest{ItemID: id, Run: run, SessionID: session, Placement: placement}
	return m.request(func(ctx context.Context) (string, error) {
		return fmt.Sprintf("resumed run %d in Ghostty (%s)", run, placement), m.opts.Resume(ctx, req)
	})
}

// editorCmd returns the editor invocation with each path as its own
// argument. The editor runs interactively in the terminal for as long as the
// user needs; only shutdown (the model's context ending) stops it and
// everything it started (see EditorCommand).
func (m *Model) editorCmd(paths ...string) *EditorCommand {
	return newEditorCommand(m.ctx, m.opts.EditorStopDelay, m.opts.Editor, paths...)
}

func (m *Model) openReport(id string, run int) tea.Cmd {
	return func() tea.Msg {
		ok, err := m.opts.Store.HasRunFile(id, run, item.ReportFile)
		if err != nil {
			return m.hintErr(fmt.Errorf("report.md: %w", err))
		}
		if !ok {
			return hintMsg{text: fmt.Sprintf("run %d has no report.md", run), err: true}
		}
		dir, err := m.opts.Store.RunPath(id, run)
		if err != nil {
			return m.hintErr(fmt.Errorf("report.md: %w", err))
		}
		return ExecMsg{
			Cmd: m.editorCmd(filepath.Join(dir, item.ReportFile)),
			After: func(err error) tea.Msg {
				return editorDoneMsg{what: "report", record: true, id: id, run: run, err: err}
			},
		}
	}
}

func (m *Model) openLogs(id string, run int) tea.Cmd {
	return func() tea.Msg {
		dir, err := m.opts.Store.RunPath(id, run)
		if err != nil {
			return m.hintErr(fmt.Errorf("run %d logs: %w", run, err))
		}
		var paths []string
		for _, name := range []string{item.OutputFile, item.StderrFile} {
			if ok, err := m.opts.Store.HasRunFile(id, run, name); err == nil && ok {
				paths = append(paths, filepath.Join(dir, name))
			}
		}
		if len(paths) == 0 {
			return hintMsg{text: fmt.Sprintf("run %d has no readable output.jsonl or stderr.log", run), err: true}
		}
		return ExecMsg{
			Cmd:   m.editorCmd(paths...),
			After: func(err error) tea.Msg { return editorDoneMsg{what: "logs", err: err} },
		}
	}
}

func (m *Model) openItemDir(id string) tea.Cmd {
	return func() tea.Msg {
		dir, err := m.opts.Store.ItemPath(id)
		if err != nil {
			return m.hintErr(fmt.Errorf("item directory: %w", err))
		}
		return ExecMsg{
			Cmd:   m.editorCmd(dir),
			After: func(err error) tea.Msg { return editorDoneMsg{what: "item directory", err: err} },
		}
	}
}

// editorDone handles an editor exit. Only a successful report open is
// recorded; the action is recorded for the run that was opened.
func (m *Model) editorDone(msg editorDoneMsg) tea.Cmd {
	if msg.err != nil {
		m.setHint(fmt.Sprintf("editor (%s): %v", msg.what, msg.err), true)
		return nil
	}
	if !msg.record {
		m.setHint("", false)
		return nil
	}
	store, now := m.opts.Store, m.opts.Now
	return func() tea.Msg {
		if _, err := store.AppendAction(msg.id, item.ActionOpenedReport, msg.run, now().UTC().Truncate(time.Second)); err != nil {
			return hintMsg{text: fmt.Sprintf("record opened report: %v", err), err: true}
		}
		return hintMsg{text: fmt.Sprintf("opened report of run %d", msg.run)}
	}
}

// browserURL returns the runbook URL, else the generator URL. Only http(s)
// URLs are opened.
func browserURL(a item.AlertInfo) (string, error) {
	u := a.RunbookURL
	if u == "" {
		u = a.GeneratorURL
	}
	if u == "" {
		return "", errors.New("the alert has no runbook or source URL")
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return "", errors.New("the alert URL is not an http(s) URL")
	}
	return u, nil
}

func (m *Model) openURL(v snapshot.ItemView) tea.Cmd {
	u, err := browserURL(v.Item.Alert)
	if err != nil {
		m.setHint(err.Error(), true)
		return nil
	}
	if m.opts.Open == nil {
		m.setHint("opening URLs is not available", true)
		return nil
	}
	return m.request(func(ctx context.Context) (string, error) {
		return "opened in the browser", m.opts.Open(ctx, u)
	})
}
