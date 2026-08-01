package serve

import (
	"encoding/json"
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

// RoleConfig configures the agent invocation for one role. Command is the
// full command line to run for this role — ekbn has no built-in agent CLI:
// it splits Command into argv, appends the prompt as the final argument, and
// execs it. An empty Command means nothing is invoked for that role at all;
// ekbn never assumes or falls back to any particular CLI. Command and
// MaxDurationMinutes are per-role, not global: a reviewer role can run a
// smaller/faster model with a tighter time budget than an implementer role.
// MaxDurationMinutes is enforced by the orchestrator killing the subprocess
// (see runAgentAttempt) — that is the one budget dimension ekbn can enforce
// itself without depending on any particular CLI's own accounting.
type RoleConfig struct {
	Prompt             string   `yaml:"prompt"`
	Tools              []string `yaml:"tools"`
	Skills             []string `yaml:"skills"`
	Command            string   `yaml:"command"`
	MaxDurationMinutes int      `yaml:"max_duration_minutes"`
}

// StageFlow is a stage sequence keyed by goal, not written into each item:
// changing a flow's Stages/MaxRounds here takes effect for every card with
// that goal without editing any existing card file.
type StageFlow struct {
	Stages    []string `yaml:"stages"`
	MaxRounds int      `yaml:"max_rounds"`
}

// defaultFlows is used for any goal not overridden in ekbn.config.yml.
var defaultFlows = map[string]StageFlow{
	"bug":      {Stages: []string{"reproduce", "fix", "verify"}, MaxRounds: 3},
	"feature":  {Stages: []string{"implement", "gates", "review"}, MaxRounds: 3},
	"refactor": {Stages: []string{"tests-frozen", "implement", "verify-behavior"}, MaxRounds: 3},
	"spike":    {Stages: []string{"research"}, MaxRounds: 0},
}

const defaultWIPLimit = 1

type Config struct {
	Theme         string                `yaml:"theme"`
	FolderName    string                `yaml:"folder-name"`
	Port          int                   `yaml:"port"`
	Prompt        string                `yaml:"prompt"`
	Verify        string                `yaml:"verify"`
	Roles         map[string]RoleConfig `yaml:"roles"`
	SecurityPaths []string              `yaml:"security-paths"`
	Flows         map[string]StageFlow  `yaml:"flows"`
	// WIPLimit caps how many tickets the orchestrator runs at once. Each
	// ticket gets its own git worktree/branch, so concurrent tickets are
	// isolated from each other; merging back into main handles a sibling
	// ticket having already advanced it (see mergeAndRemoveWorktree).
	WIPLimit int `yaml:"wip-limit"`
	// DoneOnCleanReview, if true, treats a clean code-review pass (no
	// concrete findings, and no security findings if applicable) as
	// sufficient to mark a ticket done even when it has no Acceptance
	// criteria of its own — reducing how often a human has to manually
	// promote a reviewed ticket. Off by default: without a scripted
	// acceptance check, "the reviewer had nothing to say" is not the same
	// guarantee as "a human confirmed this is right," so this is an explicit
	// trade a project opts into, not silently assumed.
	DoneOnCleanReview bool `yaml:"done-on-clean-review"`
}

// FlowFor returns the stage flow for goal, falling back to defaultFlows when
// goal is unset or not present in either the config or the defaults.
func (c Config) FlowFor(goal string) StageFlow {
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
	cfg.DoneOnCleanReview = parsed.DoneOnCleanReview
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
