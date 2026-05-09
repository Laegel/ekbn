package main

import (
	"embed"
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
	"gopkg.in/yaml.v3"
	"ekbn/model"
)

var distFSRoot fs.FS

func init() {
	var err error
	distFSRoot, err = fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatalf("failed to access embedded dist: %v", err)
	}
}

//go:embed dist
var distFS embed.FS

type Config struct {
	Theme      string `yaml:"theme"`
	FolderName string `yaml:"folder-name"`
	Port       int    `yaml:"port"`
}

func loadConfig() Config {
	cfg := Config{Theme: "dark", FolderName: "columns", Port: 0}
	data, err := os.ReadFile("config.yml")
	if err != nil {
		return cfg
	}
	var parsed Config
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		log.Printf("config.yml: %v, using defaults", err)
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
	return cfg
}

var columnsDir string

type eventBroker struct {
	mu      sync.RWMutex
	clients map[chan string]bool
}

func newEventBroker() *eventBroker {
	return &eventBroker{
		clients: make(map[chan string]bool),
	}
}

func (b *eventBroker) register(ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clients[ch] = true
}

func (b *eventBroker) unregister(ch chan string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.clients, ch)
	close(ch)
}

func (b *eventBroker) broadcast(event string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.clients {
		select {
		case ch <- event:
		default:
		}
	}
}

func (b *eventBroker) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", "*")

		ch := make(chan string, 16)
		b.register(ch)
		defer b.unregister(ch)

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case event := <-ch:
				var payload map[string]interface{}
				if err := json.Unmarshal([]byte(event), &payload); err == nil {
					if eventType, ok := payload["type"].(string); ok {
						fmt.Fprintf(w, "event: %s\n", eventType)
					}
				}
				fmt.Fprintf(w, "data: %s\n\n", event)
				flusher.Flush()
			}
		}
	}
}

func main() {
	cfg := loadConfig()
	columnsDir = getColumnsDir(cfg.FolderName)
	ensureColumns()

	broker := newEventBroker()
	watcher := newFolderWatcher(columnsDir, broker)
	defer watcher.close()

	http.Handle("/", staticServer(cfg.Theme))
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

var defaultColumnSlugs = []string{"todo", "in-progress", "review", "done"}

func ensureColumns() {
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

func staticServer(theme string) http.Handler {
	fsys := http.FileServer(http.FS(distFSRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			serveIndexWithTheme(w, r, theme)
			return
		}
		fsys.ServeHTTP(w, r)
	})
}

func serveIndexWithTheme(w http.ResponseWriter, r *http.Request, theme string) {
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

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func errorResponse(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]string{"error": message})
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
			Blocked    bool     `json:"blocked"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			errorResponse(w, http.StatusBadRequest, "Invalid JSON")
			return
		}

		if req.Title == "" {
			errorResponse(w, http.StatusBadRequest, "Title is required")
			return
		}

		filename, err := model.CreateCard(columnsDir, req.Column, req.Title, "user", req.Content, req.Categories, req.Priority, req.Role, req.Blocked)
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
		Blocked    bool     `json:"blocked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		errorResponse(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	if err := model.UpdateCard(columnsDir, column, filename, req.Title, req.Content, req.Categories, req.Priority, req.Role, req.Blocked); err != nil {
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
	prevState   map[string]fileState // filename -> {modTime, column}
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
			// File was deleted
			parts := strings.SplitN(path, string(filepath.Separator), 2)
			if len(parts) == 2 {
				filename := filepath.Base(path)
				broadcastEvent(fw.broker, "card-deleted", map[string]any{
					"filename": filename,
					"column":   prev.column,
				})
			}
		} else if !current.modTime.Equal(prev.modTime) {
			// File was modified
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
			// File was created
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
