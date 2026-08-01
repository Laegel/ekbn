package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"

	"ekbn/model"
)

// CommandSpec is an argv-list agent CLI invocation: Program is the
// executable, Args its arguments, kept separate instead of a single
// shell-string so ekbn never has to re-implement shell quoting/splitting
// itself (see BuildArgv) — a bug found live: `sh -c 'touch file.txt'` broke
// under whitespace-splitting because there was no way to keep `touch
// file.txt` together as one argument.
type CommandSpec struct {
	Program string   `yaml:"program"`
	Args    []string `yaml:"args,omitempty"`
}

// ExecutorConfig is a named, reusable agent CLI invocation — how a role's
// work actually gets run, kept separate from what capability the role
// represents. Command is an explicit argv (see CommandSpec/BuildArgv), with
// the prompt appended as the final argument before ekbn execs it — ekbn has
// no built-in agent CLI. MaxDurationMinutes is enforced by the orchestrator
// killing the subprocess (see runAgentAttempt) — the one budget dimension
// ekbn can enforce itself without depending on any particular CLI's own
// accounting. IdleTimeoutMinutes kills the agent early if it produces no
// new output (nothing written to stdout/stderr) for this many minutes —
// distinct from MaxDurationMinutes, which caps total runtime regardless of
// whether the process is actively working. A CLI that stalls (goes silent
// without crashing or finishing) is treated as a transient glitch worth
// retrying, the same bounded way a reviewer's findings cycle a ticket back
// for another attempt; a genuine MaxDurationMinutes timeout still goes
// straight to budget-exhausted for a human. 0/unset disables idle
// detection, the same convention as MaxDurationMinutes.
type ExecutorConfig struct {
	Command            CommandSpec `yaml:"command"`
	MaxDurationMinutes int         `yaml:"max_duration_minutes"`
	IdleTimeoutMinutes int         `yaml:"idle_timeout_minutes"`
}

// RoleConfig defines a capability (what the agent should do and with what
// prompt/tools/skills), not an identity — it names an Executor rather than
// embedding a command line itself, so today's opencode-backed "backend"
// role can point at a different CLI tomorrow without becoming a different
// role. MaxDurationMinutes/IdleTimeoutMinutes here are optional overrides:
// unset (0) falls back to the referenced Executor's own values, letting one
// role run a tighter or looser budget on a shared executor without
// duplicating its command line.
type RoleConfig struct {
	Prompt             string   `yaml:"prompt"`
	Tools              []string `yaml:"tools"`
	Skills             []string `yaml:"skills"`
	Executor           string   `yaml:"executor"`
	MaxDurationMinutes int      `yaml:"max_duration_minutes,omitempty"`
	IdleTimeoutMinutes int      `yaml:"idle_timeout_minutes,omitempty"`
}

// FlowState is one node in a goal's flow graph. Type selects a built-in
// stage kind ("" for a role-backed executor stage, "verify" for a command
// whose pass/fail *is* its outcome — see classifyOutcome); Role names which
// role runs an executor-backed state, falling back to the card's own Role
// when empty (preserving today's "the card picks the implementer, review-
// shaped states pick their own fixed role" split). On maps the state's
// classified Outcome to the next state's name — either another key in the
// same Flow's States, or a key in its Terminal map. An Outcome with no
// entry in On is not guessed at: the ticket blocks with a clear
// "unhandled-outcome" reason rather than silently misrouting.
type FlowState struct {
	Type            string                   `yaml:"type,omitempty"`
	Role            string                   `yaml:"role,omitempty"`
	Command         string                   `yaml:"command,omitempty"`
	ForbidTestEdits bool                     `yaml:"forbid_test_edits,omitempty"`
	On              map[model.Outcome]string `yaml:"on"`
	MaxAttempts     int                      `yaml:"max_attempts,omitempty"`
}

// TerminalState is a flow's exit point — Board is which kanban column
// (Status bucket) a card lands in when it reaches this state.
type TerminalState struct {
	Board model.Status `yaml:"board"`
}

// Flow is a state machine over typed stage outcomes, keyed by goal — not
// written into each card, so changing a flow here takes effect for every
// card with that goal without editing any existing card file. Entry is the
// state a fresh card (empty Stage) starts at.
type Flow struct {
	Entry    string                   `yaml:"entry"`
	States   map[string]FlowState     `yaml:"states"`
	Terminal map[string]TerminalState `yaml:"terminal"`
}

// defaultFlows is used for any goal not overridden in ekbn.config.yml.
var defaultFlows = map[string]Flow{
	"bug": {
		Entry: "reproduce",
		States: map[string]FlowState{
			"reproduce": {
				On: map[model.Outcome]string{
					model.OutcomeSuccess: "verify-reproduce", model.OutcomeFailed: "blocked",
					model.OutcomeFindings: "blocked", model.OutcomeAmbiguous: "blocked", model.OutcomeBlocked: "blocked",
				},
			},
			// Reaching this state with a passing verify means the bug
			// couldn't be reproduced — success here is the bad outcome,
			// simply by routing it to a block instead of forward.
			"verify-reproduce": {
				Type: "verify",
				On:   map[model.Outcome]string{model.OutcomeSuccess: "could-not-reproduce", model.OutcomeFailed: "fix"},
			},
			"fix": {
				On: map[model.Outcome]string{
					model.OutcomeSuccess: "verify-fix", model.OutcomeFailed: "blocked", model.OutcomeFindings: "blocked", model.OutcomeAmbiguous: "blocked", model.OutcomeBlocked: "blocked",
				},
			},
			"verify-fix": {
				Type: "verify",
				On:   map[model.Outcome]string{model.OutcomeSuccess: "review", model.OutcomeFailed: "fix"},
			},
			"review": {
				Type: "review",
				Role: "reviewer",
				On: map[model.Outcome]string{
					model.OutcomeSuccess: "done", model.OutcomeFindings: "fix", model.OutcomeBlocked: "blocked",
				},
			},
		},
		Terminal: map[string]TerminalState{
			"could-not-reproduce": {Board: model.StatusBlocked},
			"done":                {Board: model.StatusDone},
			"blocked":             {Board: model.StatusBlocked},
		},
	},
	"feature": {
		Entry: "implement",
		States: map[string]FlowState{
			"implement": {
				On: map[model.Outcome]string{
					model.OutcomeSuccess: "verify", model.OutcomeFailed: "blocked", model.OutcomeFindings: "blocked", model.OutcomeAmbiguous: "blocked", model.OutcomeBlocked: "blocked",
				},
			},
			"verify": {
				Type: "verify",
				On:   map[model.Outcome]string{model.OutcomeSuccess: "review", model.OutcomeFailed: "implement"},
			},
			"review": {
				Type: "review",
				Role: "reviewer",
				On: map[model.Outcome]string{
					model.OutcomeSuccess: "done", model.OutcomeFindings: "implement", model.OutcomeBlocked: "blocked",
				},
			},
		},
		Terminal: map[string]TerminalState{
			"done":    {Board: model.StatusDone},
			"blocked": {Board: model.StatusBlocked},
		},
	},
	"refactor": {
		Entry: "tests-frozen",
		States: map[string]FlowState{
			// ForbidTestEdits is the mechanical "tests must stay frozen
			// during a refactor" check — any touched test file is a
			// blocked outcome regardless of what the verify command itself
			// says, exactly as before, now expressed as a state property
			// instead of a goal=="refactor"-shaped if-branch.
			"tests-frozen": {
				ForbidTestEdits: true,
				On: map[model.Outcome]string{
					model.OutcomeSuccess: "verify-behavior", model.OutcomeFailed: "blocked", model.OutcomeFindings: "blocked", model.OutcomeAmbiguous: "blocked", model.OutcomeBlocked: "blocked",
				},
			},
			"verify-behavior": {
				Type: "verify",
				On:   map[model.Outcome]string{model.OutcomeSuccess: "review", model.OutcomeFailed: "tests-frozen"},
			},
			"review": {
				Type: "review",
				Role: "reviewer",
				On: map[model.Outcome]string{
					model.OutcomeSuccess: "done", model.OutcomeFindings: "tests-frozen", model.OutcomeBlocked: "blocked",
				},
			},
		},
		Terminal: map[string]TerminalState{
			"done":    {Board: model.StatusDone},
			"blocked": {Board: model.StatusBlocked},
		},
	},
	"spike": {
		Entry: "research",
		States: map[string]FlowState{
			"research": {On: map[model.Outcome]string{model.OutcomeSuccess: "done"}},
		},
		Terminal: map[string]TerminalState{
			"done": {Board: model.StatusDone},
		},
	},
}

const defaultWIPLimit = 1

type Config struct {
	Theme         string                    `yaml:"theme"`
	FolderName    string                    `yaml:"folder-name"`
	Port          int                       `yaml:"port"`
	Prompt        string                    `yaml:"prompt"`
	Verify        string                    `yaml:"verify"`
	Executors     map[string]ExecutorConfig `yaml:"executors"`
	Roles         map[string]RoleConfig     `yaml:"roles"`
	SecurityPaths []string                  `yaml:"security-paths"`
	Flows         map[string]Flow           `yaml:"flows"`
	// WIPLimit caps how many tickets the orchestrator runs at once. Each
	// ticket gets its own git worktree/branch, so concurrent tickets are
	// isolated from each other; merging back into main handles a sibling
	// ticket having already advanced it (see mergeAndRemoveWorktree).
	WIPLimit int `yaml:"wip-limit"`
}

// ErrUnknownExecutor is returned by ResolveExecutor when a role names an
// executor that isn't configured — a real config mistake, distinct from a
// role simply having no executor set at all (which just means nothing is
// invoked for it, same as an empty Command meant before executors existed).
var ErrUnknownExecutor = errors.New("role references an unconfigured executor")

// ResolveExecutor looks up role in roles (falling back to roles["default"],
// reporting fellBack=true) and resolves its named Executor, applying the
// role's own MaxDurationMinutes/IdleTimeoutMinutes as overrides when set.
// This is the one place role-with-executor resolution happens — both the
// orchestrator and specify used to duplicate this lookup-with-fallback
// logic separately (once for a role's Command, once for its own copy of the
// same fallback rule); now there's a single definition. If roles is empty
// entirely (unconfigured), returns zero values with fellBack=false,
// preserving pre-executor behavior exactly (an empty Command downstream
// meant "nothing configured," not an error). A role naming an executor that
// isn't in cfg.Executors is ErrUnknownExecutor — a real mistake to surface,
// not something to silently fall back from.
func ResolveExecutor(role string, cfg Config) (ec ExecutorConfig, rc RoleConfig, fellBack bool, err error) {
	if len(cfg.Roles) == 0 {
		return ExecutorConfig{}, RoleConfig{}, false, nil
	}
	if role != "" {
		if found, ok := cfg.Roles[role]; ok {
			rc = found
		} else {
			rc, fellBack = cfg.Roles["default"], true
		}
	} else {
		rc, fellBack = cfg.Roles["default"], true
	}
	if rc.Executor == "" {
		return ExecutorConfig{}, rc, fellBack, nil
	}
	ec, ok := cfg.Executors[rc.Executor]
	if !ok {
		return ExecutorConfig{}, rc, fellBack, fmt.Errorf("%w: %q", ErrUnknownExecutor, rc.Executor)
	}
	if rc.MaxDurationMinutes > 0 {
		ec.MaxDurationMinutes = rc.MaxDurationMinutes
	}
	if rc.IdleTimeoutMinutes > 0 {
		ec.IdleTimeoutMinutes = rc.IdleTimeoutMinutes
	}
	return ec, rc, fellBack, nil
}

// FlowFor returns the stage flow for goal, falling back to defaultFlows when
// goal is unset or not present in either the config or the defaults.
func (c Config) FlowFor(goal string) Flow {
	if f, ok := c.Flows[goal]; ok {
		return f
	}
	return defaultFlows[goal]
}

func configPath() string {
	for _, name := range []string{"ekbn.config.yml", "ekbn.config.yaml"} {
		if _, err := os.Stat(name); err == nil {
			return name
		}
	}
	return ""
}

func LoadConfig() Config {
	cfg := Config{Theme: "dark", FolderName: "columns", Port: 0, WIPLimit: defaultWIPLimit}
	path := configPath()
	if path == "" {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	var parsed Config
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		log.Printf("%s: %v, using defaults", path, err)
		return cfg
	}
	if parsed.Theme != "" {
		cfg.Theme = parsed.Theme
	}
	if parsed.FolderName != "" {
		cfg.FolderName = parsed.FolderName
	}
	if parsed.Port > 0 {
		cfg.Port = parsed.Port
	}
	if parsed.Prompt != "" {
		cfg.Prompt = parsed.Prompt
	}
	if parsed.Verify != "" {
		cfg.Verify = parsed.Verify
	}
	if len(parsed.Executors) > 0 {
		cfg.Executors = parsed.Executors
	}
	if len(parsed.Roles) > 0 {
		cfg.Roles = parsed.Roles
	}
	if len(parsed.SecurityPaths) > 0 {
		cfg.SecurityPaths = parsed.SecurityPaths
	}
	if len(parsed.Flows) > 0 {
		cfg.Flows = parsed.Flows
	}
	if parsed.WIPLimit > 0 {
		cfg.WIPLimit = parsed.WIPLimit
	}
	return cfg
}

func getColumnsDir(folderName string) string {
	dir := os.Getenv("EKB_COLUMNS")
	if dir != "" {
		return dir
	}
	abs, err := filepath.Abs(folderName)
	if err != nil {
		log.Fatal(err)
	}
	return abs
}

var defaultColumnSlugs = []string{
	string(model.StatusTodo), string(model.StatusInProgress), string(model.StatusReview),
	string(model.StatusDone), string(model.StatusBlocked), string(model.StatusBudgetExhausted),
}

func ensureColumns(columnsDir string) {
	existing, _ := os.ReadDir(columnsDir)
	existingSlugs := make(map[string]bool)
	for _, e := range existing {
		if !e.IsDir() {
			continue
		}
		def := model.DirToColumnDef(e.Name())
		if def != nil {
			existingSlugs[def.Slug] = true
		}
	}

	for _, col := range defaultColumnSlugs {
		if existingSlugs[col] {
			continue
		}
		def := model.ColumnDef{Slug: col}
		for _, d := range model.DefaultColumns {
			if d.Slug == col {
				def = d
				break
			}
		}
		path := filepath.Join(columnsDir, fmt.Sprintf("%d-%s", def.Index, def.Slug))
		if _, err := os.Stat(path); os.IsNotExist(err) {
			os.MkdirAll(path, 0755)
		}
	}
}

type wsClient struct {
	conn *websocket.Conn
	send chan []byte
}

type eventBroker struct {
	mu      sync.RWMutex
	clients map[*wsClient]bool
}

func newEventBroker() *eventBroker {
	return &eventBroker{
		clients: make(map[*wsClient]bool),
	}
}

func (b *eventBroker) register(client *wsClient) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[client] = true
}

func (b *eventBroker) unregister(client *wsClient) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.clients[client]; ok {
		delete(b.clients, client)
		close(client.send)
	}
}

func (b *eventBroker) broadcast(event string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for client := range b.clients {
		select {
		case client.send <- []byte(event):
		default:
			log.Printf("WebSocket client send buffer full, dropping message")
		}
	}
}

var wsUpgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (b *eventBroker) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := wsUpgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("WebSocket upgrade error: %v", err)
			return
		}

		client := &wsClient{
			conn: conn,
			send: make(chan []byte, 16),
		}
		b.register(client)

		go b.writePump(client)
		b.readPump(client)
	}
}

func (b *eventBroker) readPump(client *wsClient) {
	defer func() {
		b.unregister(client)
		client.conn.Close()
	}()

	client.conn.SetReadLimit(512)
	client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	client.conn.SetPongHandler(func(string) error {
		client.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		if _, _, err := client.conn.ReadMessage(); err != nil {
			break
		}
	}
}

func (b *eventBroker) writePump(client *wsClient) {
	defer client.conn.Close()

	for msg := range client.send {
		if err := client.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

type fileState struct {
	modTime time.Time
	column  string
}

type folderWatcher struct {
	watcher     *fsnotify.Watcher
	ticker      *time.Timer
	mu          sync.Mutex
	broker      *eventBroker
	lastAPITime time.Time
	prevState   map[string]fileState
}

func newFolderWatcher(dir string, broker *eventBroker) *folderWatcher {
	fw := &folderWatcher{
		broker:    broker,
		ticker:    time.NewTimer(0),
		prevState: make(map[string]fileState),
	}
	fw.ticker.Stop()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("fsnotify error: %v", err)
		return fw
	}
	fw.watcher = w

	addWatchDirs(w, dir)
	fw.prevState = fw.snapshotState(dir)

	go fw.watchLoop()

	return fw
}

func (fw *folderWatcher) notifyAPIEvent() {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	fw.lastAPITime = time.Now()
	fw.prevState = fw.snapshotState(columnsDir)
}

func (fw *folderWatcher) trigger() {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if time.Since(fw.lastAPITime) < 500*time.Millisecond {
		return
	}
	fw.ticker.Reset(300 * time.Millisecond)
}

func (fw *folderWatcher) snapshotState(dir string) map[string]fileState {
	state := make(map[string]fileState)
	columns, _ := model.ListColumns(dir)
	for _, col := range columns {
		dirName := fmt.Sprintf("%d-%s", col.Index, col.Slug)
		pattern := filepath.Join(dir, dirName, "*.md")
		files, _ := filepath.Glob(pattern)
		for _, f := range files {
			info, err := os.Stat(f)
			if err != nil {
				continue
			}
			rel, _ := filepath.Rel(dir, f)
			state[rel] = fileState{modTime: info.ModTime(), column: dirName}
		}
	}
	return state
}

func (fw *folderWatcher) watchLoop() {
	for {
		select {
		case event, ok := <-fw.watcher.Events:
			if !ok {
				return
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			if event.Op&fsnotify.Create != 0 {
				addWatchDirs(fw.watcher, event.Name)
			}
			fw.trigger()
		case _, ok := <-fw.watcher.Errors:
			if !ok {
				return
			}
		case <-fw.ticker.C:
			fw.detectChanges()
		}
	}
}

func (fw *folderWatcher) detectChanges() {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if time.Since(fw.lastAPITime) < 500*time.Millisecond {
		return
	}

	currentState := fw.snapshotState(columnsDir)

	for path, prev := range fw.prevState {
		if current, exists := currentState[path]; !exists {
			parts := strings.SplitN(path, string(filepath.Separator), 2)
			if len(parts) == 2 {
				filename := filepath.Base(path)
				broadcastEvent(fw.broker, "card-deleted", map[string]any{
					"filename": filename,
					"column":   prev.column,
				})
			}
		} else if !current.modTime.Equal(prev.modTime) {
			parts := strings.SplitN(path, string(filepath.Separator), 2)
			if len(parts) == 2 {
				card, _ := model.ReadCard(filepath.Join(columnsDir, path), parts[0])
				broadcastEvent(fw.broker, "card-updated", map[string]any{
					"filename": filepath.Base(path),
					"column":   parts[0],
					"card":     card,
				})
			}
		}
	}

	for path := range currentState {
		if _, exists := fw.prevState[path]; !exists {
			parts := strings.SplitN(path, string(filepath.Separator), 2)
			if len(parts) == 2 {
				card, _ := model.ReadCard(filepath.Join(columnsDir, path), parts[0])
				broadcastEvent(fw.broker, "card-created", map[string]any{
					"filename": filepath.Base(path),
					"column":   parts[0],
					"card":     card,
				})
			}
		}
	}

	fw.prevState = currentState
}

func (fw *folderWatcher) close() {
	if fw.watcher != nil {
		fw.watcher.Close()
	}
}

func addWatchDirs(w *fsnotify.Watcher, dir string) {
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		w.Add(path)
		return nil
	})
}

var columnsDir string

func broadcastEvent(broker *eventBroker, eventType string, data any) {
	payload := map[string]any{"type": eventType}
	if data != nil {
		for k, v := range data.(map[string]any) {
			payload[k] = v
		}
	}
	raw, _ := json.Marshal(payload)
	log.Printf("Broadcasting event %s", eventType)
	broker.broadcast(string(raw))
}

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func errorResponse(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]string{"error": message})
}

func staticServer(theme string, distFSRoot fs.FS) http.Handler {
	fsys := http.FileServer(http.FS(distFSRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			serveIndexWithTheme(w, r, theme, distFSRoot)
			return
		}
		fsys.ServeHTTP(w, r)
	})
}

func serveIndexWithTheme(w http.ResponseWriter, r *http.Request, theme string, distFSRoot fs.FS) {
	data, err := fs.ReadFile(distFSRoot, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	content := strings.Replace(string(data), `data-theme="dark"`, fmt.Sprintf(`data-theme="%s"`, theme), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(content))
}

func handleCustomCSS(w http.ResponseWriter, r *http.Request) {
	paths := []string{"custom.css"}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			http.ServeFile(w, r, p)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleColumns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listColumns(w, r)
	case http.MethodPost:
		createColumn(w, r)
	default:
		errorResponse(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func listColumns(w http.ResponseWriter, r *http.Request) {
	columns, err := model.ListColumns(columnsDir)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, columns)
}

func createColumn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if req.ID == "" || req.Name == "" {
		errorResponse(w, http.StatusBadRequest, "ID and name are required")
		return
	}

	dirName, idx, err := model.CreateColumn(columnsDir, req.ID, req.Name)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	jsonResponse(w, http.StatusCreated, map[string]interface{}{"id": dirName, "name": req.Name, "index": idx})
}

func handleCards(broker *eventBroker, watcher *folderWatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			errorResponse(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}

		var req struct {
			Column     string   `json:"column"`
			Title      string   `json:"title"`
			Content    string   `json:"content"`
			Categories []string `json:"categories"`
			Priority   int      `json:"priority"`
			Role       string   `json:"role"`
			Goal       string   `json:"goal"`
			DependsOn  []string `json:"depends_on"`
			Acceptance string   `json:"acceptance"`
			Unresolved string   `json:"unresolved"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorResponse(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		if req.Title == "" {
			errorResponse(w, http.StatusBadRequest, "Title is required")
			return
		}

		filename, err := model.CreateCard(columnsDir, req.Column, model.CardFields{
			Title:      req.Title,
			Author:     "user",
			Content:    req.Content,
			Categories: req.Categories,
			Priority:   req.Priority,
			Role:       req.Role,
			Goal:       req.Goal,
			DependsOn:  req.DependsOn,
			Acceptance: req.Acceptance,
			Unresolved: req.Unresolved,
		})
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}

		watcher.notifyAPIEvent()
		card, _ := model.ReadCard(filepath.Join(columnsDir, req.Column, filename), req.Column)
		broadcastEvent(broker, "card-created", map[string]any{
			"filename": filename,
			"column":   req.Column,
			"card":     card,
		})

		jsonResponse(w, http.StatusCreated, map[string]string{"filename": filename})
	}
}

func handleCard(broker *eventBroker, watcher *folderWatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/card/")
		parts := strings.SplitN(path, "/", 2)

		if len(parts) != 2 {
			errorResponse(w, http.StatusBadRequest, "Invalid path. Use /api/card/{column}/{filename}")
			return
		}

		column := parts[0]
		filename := parts[1]

		if strings.HasSuffix(filename, "/comment") {
			if r.Method != http.MethodPost {
				errorResponse(w, http.StatusMethodNotAllowed, "Method not allowed")
				return
			}
			handleAddComment(w, r, column, strings.TrimSuffix(filename, "/comment"), broker, watcher)
			return
		}

		switch r.Method {
		case http.MethodGet:
			handleGetCard(w, r, column, filename)
		case http.MethodPut:
			handleUpdateCard(w, r, column, filename, broker, watcher)
		case http.MethodDelete:
			handleDeleteCard(w, column, filename, broker, watcher)
		case http.MethodPost:
			handleMoveCard(w, r, column, filename, broker, watcher)
		default:
			errorResponse(w, http.StatusMethodNotAllowed, "Method not allowed")
		}
	}
}

func handleGetCard(w http.ResponseWriter, r *http.Request, column, filename string) {
	card, err := model.ReadCard(filepath.Join(columnsDir, column, filename), column)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "Card not found")
		return
	}
	jsonResponse(w, http.StatusOK, card)
}

func handleUpdateCard(w http.ResponseWriter, r *http.Request, column, filename string, broker *eventBroker, watcher *folderWatcher) {
	var req struct {
		Title      string   `json:"title"`
		Content    string   `json:"content"`
		Categories []string `json:"categories"`
		Priority   int      `json:"priority"`
		Role       string   `json:"role"`
		Goal       string   `json:"goal"`
		DependsOn  []string `json:"depends_on"`
		Acceptance string   `json:"acceptance"`
		Unresolved string   `json:"unresolved"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := model.UpdateCard(columnsDir, column, filename, model.CardFields{
		Title:      req.Title,
		Content:    req.Content,
		Categories: req.Categories,
		Priority:   req.Priority,
		Role:       req.Role,
		Goal:       req.Goal,
		DependsOn:  req.DependsOn,
		Acceptance: req.Acceptance,
		Unresolved: req.Unresolved,
	}); err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	watcher.notifyAPIEvent()
	card, _ := model.ReadCard(filepath.Join(columnsDir, column, filename), column)
	broadcastEvent(broker, "card-updated", map[string]any{
		"filename": filename,
		"column":   column,
		"card":     card,
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleDeleteCard(w http.ResponseWriter, column, filename string, broker *eventBroker, watcher *folderWatcher) {
	if err := model.DeleteCard(columnsDir, column, filename); err != nil {
		errorResponse(w, http.StatusNotFound, "Card not found")
		return
	}

	watcher.notifyAPIEvent()
	broadcastEvent(broker, "card-deleted", map[string]any{
		"filename": filename,
		"column":   column,
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleAddComment(w http.ResponseWriter, r *http.Request, column, filename string, broker *eventBroker, watcher *folderWatcher) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if req.Text == "" {
		errorResponse(w, http.StatusBadRequest, "Text is required")
		return
	}

	if err := model.AddComment(columnsDir, column, filename, "user", req.Text); err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	watcher.notifyAPIEvent()
	card, _ := model.ReadCard(filepath.Join(columnsDir, column, filename), column)
	broadcastEvent(broker, "card-updated", map[string]any{
		"filename": filename,
		"column":   column,
		"card":     card,
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleMoveCard(w http.ResponseWriter, r *http.Request, column, filename string, broker *eventBroker, watcher *folderWatcher) {
	var req struct {
		ToColumn string `json:"toColumn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := model.MoveCard(columnsDir, column, req.ToColumn, filename); err != nil {
		errorResponse(w, http.StatusInternalServerError, err.Error())
		return
	}

	watcher.notifyAPIEvent()
	card, _ := model.ReadCard(filepath.Join(columnsDir, req.ToColumn, filename), req.ToColumn)
	broadcastEvent(broker, "card-moved", map[string]any{
		"filename":   filename,
		"fromColumn": column,
		"toColumn":   req.ToColumn,
		"card":       card,
	})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleColumn(broker *eventBroker, watcher *folderWatcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			errorResponse(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}

		slug := strings.TrimPrefix(r.URL.Path, "/api/column/")

		var req struct {
			BeforeSlug string `json:"beforeSlug"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorResponse(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		if err := model.ReorderColumn(columnsDir, slug, req.BeforeSlug); err != nil {
			errorResponse(w, http.StatusInternalServerError, err.Error())
			return
		}

		watcher.notifyAPIEvent()
		broadcastEvent(broker, "column-reordered", nil)

		jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func Run(distFS fs.FS) {
	if _, err := fs.Stat(distFS, "index.html"); err != nil {
		log.Fatalf("UI assets not found: %v. Build the frontend with 'npm run build' or use --dev if running locally", err)
	}
	cfg := LoadConfig()
	columnsDir = getColumnsDir(cfg.FolderName)
	ensureColumns(columnsDir)

	broker := newEventBroker()
	watcher := newFolderWatcher(columnsDir, broker)
	defer watcher.close()

	http.Handle("/", staticServer(cfg.Theme, distFS))
	http.HandleFunc("/custom.css", handleCustomCSS)
	http.HandleFunc("/api/columns", handleColumns)
	http.HandleFunc("/api/cards", handleCards(broker, watcher))
	http.HandleFunc("/api/card/", handleCard(broker, watcher))
	http.HandleFunc("/api/column/", handleColumn(broker, watcher))
	http.HandleFunc("/api/events", broker.handler())

	addr := os.Getenv("PORT")
	if addr == "" && cfg.Port > 0 {
		addr = fmt.Sprintf(":%d", cfg.Port)
	} else if addr != "" {
		addr = ":" + addr
	} else {
		addr = ":0"
	}

	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("ekbn server listening on http://localhost:%d", l.Addr().(*net.TCPAddr).Port)
	log.Printf("Columns directory: %s", columnsDir)
	log.Fatal(http.Serve(l, nil))
}
