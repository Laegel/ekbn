package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Comment struct {
	Author  string `yaml:"author" json:"author"`
	Created string `yaml:"created" json:"created"`
	Text    string `yaml:"text" json:"text"`
}

// Status is the card's place in the work-item lifecycle. It is the single
// field that determines state — nothing outside the frontmatter (directory,
// filename, position) is authoritative. The directory a card's file lives in
// is a view kept in sync with Status by TransitionStatus/MoveCard, not the
// other way around.
type Status string

const (
	StatusTodo            Status = "todo"
	StatusInProgress      Status = "in-progress"
	StatusReview          Status = "review"
	StatusDone            Status = "done"
	StatusBlocked         Status = "blocked"
	StatusBudgetExhausted Status = "budget-exhausted"
)

// ValidStatuses is the single source of truth for which statuses exist. UI
// column creation is validated against this set so the set of valid states
// can't diverge from what the domain model declares.
var ValidStatuses = []Status{
	StatusTodo, StatusInProgress, StatusReview, StatusDone, StatusBlocked, StatusBudgetExhausted,
}

func IsValidStatus(s Status) bool {
	for _, v := range ValidStatuses {
		if v == s {
			return true
		}
	}
	return false
}

type Card struct {
	ID           string    `yaml:"id" json:"id"`
	Title        string    `yaml:"title" json:"title"`
	Author       string    `yaml:"author" json:"author"`
	Created      string    `yaml:"created" json:"created"`
	Updated      string    `yaml:"updated" json:"updated"`
	Priority     int       `yaml:"priority" json:"priority"`
	Role         string    `yaml:"role" json:"role"`
	Goal         string    `yaml:"goal" json:"goal"`
	Spec         string    `yaml:"spec" json:"spec"`
	Status       Status    `yaml:"status" json:"status"`
	Stage        string    `yaml:"stage" json:"stage"`
	Round        int       `yaml:"round" json:"round"`
	Acceptance   string    `yaml:"acceptance" json:"acceptance"`
	DependsOn    []string  `yaml:"depends_on" json:"depends_on"`
	Unresolved   string    `yaml:"unresolved,omitempty" json:"unresolved"`
	Reason       string    `yaml:"reason" json:"reason"`
	LeaseOwner   string    `yaml:"lease_owner,omitempty" json:"lease_owner"`
	LeaseExpires string    `yaml:"lease_expires,omitempty" json:"lease_expires"`
	AttemptRef   string    `yaml:"attempt_ref,omitempty" json:"attempt_ref"`
	BaseSHA      string    `yaml:"base_sha,omitempty" json:"base_sha"`
	Checkpoint   string    `yaml:"checkpoint,omitempty" json:"checkpoint"`
	Worktree     string    `yaml:"worktree,omitempty" json:"worktree"`
	Security     bool      `yaml:"security,omitempty" json:"security"`
	Categories   []string  `yaml:"categories" json:"categories"`
	Comments     []Comment `yaml:"comments" json:"comments"`
	Content      string    `yaml:"-" json:"content"`
	Filename     string    `yaml:"-" json:"filename"`
	Column       string    `yaml:"-" json:"column"`
}

type Column struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Index int    `json:"index"`
	Name  string `json:"name"`
	Cards []Card `json:"cards"`
}

type ColumnDef struct {
	Index int
	Slug  string
	Name  string
}

// DefaultColumns are the derived-view folders, one per Status. "stalled" no
// longer exists as its own bucket: every non-success outcome now carries a
// real Status (blocked or budget-exhausted) instead of collapsing into a
// single folder distinguished only by a free-text reason.
var DefaultColumns = []ColumnDef{
	{100, string(StatusTodo), "To Do"},
	{200, string(StatusInProgress), "In Progress"},
	{250, string(StatusReview), "Review"},
	{300, string(StatusDone), "Done"},
	{400, string(StatusBlocked), "Blocked"},
	{450, string(StatusBudgetExhausted), "Budget Exhausted"},
}

var dirPattern = regexp.MustCompile(`^(\d+)-(.+)$`)

func DirToColumnDef(name string) *ColumnDef {
	matches := dirPattern.FindStringSubmatch(name)
	if matches == nil {
		return nil
	}
	idx, _ := strconv.Atoi(matches[1])
	slug := matches[2]
	return &ColumnDef{Index: idx, Slug: slug, Name: slug}
}

func ListColumns(baseDir string) ([]Column, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, err
	}

	colDefs := make([]ColumnDef, 0)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		def := DirToColumnDef(e.Name())
		if def == nil {
			continue
		}
		nameFile := filepath.Join(baseDir, e.Name(), "_name")
		if data, err := os.ReadFile(nameFile); err == nil {
			def.Name = strings.TrimSpace(string(data))
		} else {
			for _, d := range DefaultColumns {
				if d.Slug == def.Slug {
					def.Name = d.Name
					break
				}
			}
			if def.Name == "" {
				def.Name = strings.Title(strings.ReplaceAll(def.Slug, "-", " "))
			}
		}
		colDefs = append(colDefs, *def)
	}

	sort.Slice(colDefs, func(i, j int) bool {
		return colDefs[i].Index < colDefs[j].Index
	})

	columns := make([]Column, 0, len(colDefs))
	for _, def := range colDefs {
		dirName := fmt.Sprintf("%d-%s", def.Index, def.Slug)
		colPath := filepath.Join(baseDir, dirName)
		cards, err := listCards(colPath, dirName)
		if err != nil {
			return nil, fmt.Errorf("column %s: %w", dirName, err)
		}
		columns = append(columns, Column{
			ID:    dirName,
			Slug:  def.Slug,
			Index: def.Index,
			Name:  def.Name,
			Cards: cards,
		})
	}
	return columns, nil
}

// findColumnDirBySlug returns the on-disk directory name (e.g. "400-blocked")
// currently backing the given status slug, or "" if no such column exists yet.
func findColumnDirBySlug(baseDir, slug string) (string, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		def := DirToColumnDef(e.Name())
		if def != nil && def.Slug == slug {
			return e.Name(), nil
		}
	}
	return "", nil
}

// CreateColumn creates a new column. The slug must be one of ValidStatuses:
// columns are the view of the status enum, not an independent namespace the
// UI can invent values into — inventing a column that isn't a real status
// would let a card sit in a folder no code has any status semantics for.
func CreateColumn(baseDir string, slug string, name string) (string, int, error) {
	if !IsValidStatus(Status(slug)) {
		return "", 0, fmt.Errorf("%q is not a valid status (must be one of %v)", slug, ValidStatuses)
	}
	idx := NextIndex(baseDir)
	dirName := fmt.Sprintf("%d-%s", idx, slug)
	path := filepath.Join(baseDir, dirName)

	if err := os.MkdirAll(path, 0755); err != nil {
		return "", 0, err
	}
	if err := os.WriteFile(filepath.Join(path, "_name"), []byte(name), 0644); err != nil {
		return "", 0, err
	}
	return dirName, idx, nil
}

func NextIndex(baseDir string) int {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return 100
	}
	maxIdx := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		def := DirToColumnDef(e.Name())
		if def != nil && def.Index > maxIdx {
			maxIdx = def.Index
		}
	}
	return maxIdx + 100
}

func listCards(dir string, column string) ([]Card, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	cards := make([]Card, 0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		card, err := ReadCard(filepath.Join(dir, e.Name()), column)
		if err != nil {
			continue
		}
		cards = append(cards, card)
	}
	return cards, nil
}

func ReadCard(path string, column string) (Card, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Card{}, err
	}

	content := string(data)
	var frontmatterStr string
	var body string

	if strings.HasPrefix(content, "---") {
		parts := strings.SplitN(content[3:], "---", 2)
		if len(parts) == 2 {
			frontmatterStr = strings.TrimSpace(parts[0])
			body = strings.TrimSpace(parts[1])
		}
	}

	var card Card
	if frontmatterStr != "" {
		if err := yaml.Unmarshal([]byte(frontmatterStr), &card); err != nil {
			return Card{}, err
		}
	}

	card.Content = body

	card.Filename = filepath.Base(path)
	card.Column = column
	return card, nil
}

func writeCard(path string, card Card) error {
	fmData, err := yaml.Marshal(&card)
	if err != nil {
		return err
	}
	fileContent := fmt.Sprintf("---\n%s---\n\n%s", string(fmData), card.Content)
	return os.WriteFile(path, []byte(fileContent), 0644)
}

// SaveCard writes back a Card whose fields the caller has already mutated in
// memory (e.g. the orchestrator advancing Stage/Round or recording a lease).
// It does not move the file — use TransitionStatus for anything that changes
// Status, since only that function knows to keep the derived-view folder in
// sync.
func SaveCard(baseDir, column, filename string, card Card) error {
	return writeCard(filepath.Join(baseDir, column, filename), card)
}

// genID returns a stable identifier independent of the card's title, so
// depends_on references and renames of the title never break each other.
func genID() string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// CardFields is the set of author-editable fields a card can be created or
// updated with, grouped into a struct rather than positional parameters —
// this list has grown three times (blocked -> goal/depends_on/acceptance ->
// unresolved) and another positional parameter would be unreadable at call
// sites.
type CardFields struct {
	Title      string
	Author     string
	Content    string
	Categories []string
	Priority   int
	Role       string
	Spec       string
	Goal       string
	DependsOn  []string
	Acceptance string
	Unresolved string
}

func CreateCard(baseDir string, column string, f CardFields) (string, error) {
	now := time.Now().Format("2006-01-02T15:04:05Z07:00")

	status := StatusTodo
	if def := DirToColumnDef(column); def != nil && IsValidStatus(Status(def.Slug)) {
		status = Status(def.Slug)
	}

	card := Card{
		ID:         genID(),
		Title:      f.Title,
		Author:     f.Author,
		Created:    now,
		Updated:    now,
		Priority:   f.Priority,
		Role:       f.Role,
		Goal:       f.Goal,
		Spec:       f.Spec,
		Status:     status,
		DependsOn:  f.DependsOn,
		Acceptance: f.Acceptance,
		Unresolved: f.Unresolved,
		Categories: f.Categories,
		Content:    f.Content,
	}

	filename := fmt.Sprintf("%s.md", slugify(f.Title))
	path := filepath.Join(baseDir, column, filename)
	if err := writeCard(path, card); err != nil {
		return "", err
	}
	return filename, nil
}

// UpdateCard edits a card's descriptive content. It deliberately cannot
// change Status: state transitions are a distinct operation (TransitionStatus)
// with their own derived-view bookkeeping, not a side effect of an ordinary
// content edit. Spec and Author in f are ignored: Spec is provenance set
// once at creation by the planning agent, not an editable field, and Author
// is set once at creation too.
func UpdateCard(baseDir string, column string, filename string, f CardFields) error {
	path := filepath.Join(baseDir, column, filename)
	card, err := ReadCard(path, column)
	if err != nil {
		return err
	}

	card.Title = f.Title
	card.Content = f.Content
	card.Categories = f.Categories
	card.Priority = f.Priority
	card.Role = f.Role
	card.Goal = f.Goal
	card.DependsOn = f.DependsOn
	card.Acceptance = f.Acceptance
	card.Unresolved = f.Unresolved
	card.Updated = time.Now().Format("2006-01-02T15:04:05Z07:00")
	return writeCard(path, card)
}

func AddComment(baseDir string, column string, filename string, author string, text string) error {
	path := filepath.Join(baseDir, column, filename)
	card, err := ReadCard(path, column)
	if err != nil {
		return err
	}

	card.Comments = append(card.Comments, Comment{
		Author:  author,
		Created: time.Now().Format("2006-01-02T15:04:05Z07:00"),
		Text:    text,
	})

	return writeCard(path, card)
}

func DeleteCard(baseDir string, column string, filename string) error {
	path := filepath.Join(baseDir, column, filename)
	return os.Remove(path)
}

// TransitionStatus is the single place a card's Status changes. It writes the
// new Status (and, if reason is non-empty, Reason) into the card's own
// frontmatter — the field edit is the state change — and then moves the file
// into whichever folder currently backs that status, purely so the existing
// column-based board view stays in sync. If no folder backs that status yet,
// the card's frontmatter is still updated; the caller may end up with a card
// whose folder doesn't match its status until that column is created, but the
// frontmatter remains authoritative regardless.
func TransitionStatus(baseDir, column, filename string, newStatus Status, reason string) (newColumn string, err error) {
	if !IsValidStatus(newStatus) {
		return "", fmt.Errorf("%q is not a valid status", newStatus)
	}
	path := filepath.Join(baseDir, column, filename)
	card, err := ReadCard(path, column)
	if err != nil {
		return "", err
	}
	card.Status = newStatus
	if reason != "" {
		card.Reason = reason
	}
	card.Updated = time.Now().Format("2006-01-02T15:04:05Z07:00")
	if err := writeCard(path, card); err != nil {
		return "", err
	}

	targetDir, err := findColumnDirBySlug(baseDir, string(newStatus))
	if err != nil {
		return "", err
	}
	if targetDir == "" || targetDir == column {
		return column, nil
	}
	dst := filepath.Join(baseDir, targetDir, filename)
	if err := os.Rename(path, dst); err != nil {
		return "", err
	}
	return targetDir, nil
}

// MoveCard is the UI/API entry point for dragging a card to another column.
// Since every column is a view over a Status, moving a card is a status
// transition; the destination column's slug must therefore name a real
// status.
func MoveCard(baseDir string, fromColumn string, toColumn string, filename string) error {
	def := DirToColumnDef(toColumn)
	if def == nil {
		return fmt.Errorf("invalid destination column %q", toColumn)
	}
	_, err := TransitionStatus(baseDir, fromColumn, filename, Status(def.Slug), "")
	return err
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	result := b.String()
	if result == "" {
		result = "untitled"
	}
	return result
}

func ReorderColumn(baseDir string, columnSlug string, beforeColumnSlug string) error {
	columns, err := ListColumns(baseDir)
	if err != nil {
		return err
	}

	var colIdx int
	targetIdx := len(columns)
	for i, c := range columns {
		if c.Slug == columnSlug {
			colIdx = i
		}
		if c.Slug == beforeColumnSlug {
			targetIdx = i
		}
	}

	if colIdx == targetIdx || targetIdx == colIdx+1 {
		return nil
	}

	moved := columns[colIdx]
	rest := make([]Column, 0, len(columns)-1)
	for i, c := range columns {
		if i != colIdx {
			rest = append(rest, c)
		}
	}

	adjustedTarget := targetIdx
	if targetIdx > colIdx {
		adjustedTarget--
	}

	newOrder := make([]Column, 0, len(columns))
	inserted := false
	for i, c := range rest {
		if i == adjustedTarget {
			newOrder = append(newOrder, moved)
			inserted = true
		}
		newOrder = append(newOrder, c)
	}
	if !inserted {
		newOrder = append(newOrder, moved)
	}

	for i, c := range newOrder {
		def := DirToColumnDef(c.ID)
		if def == nil {
			continue
		}
		newIndex := (i + 1) * 100
		newDirName := fmt.Sprintf("%d-%s", newIndex, def.Slug)
		if c.ID == newDirName {
			continue
		}
		oldDir := filepath.Join(baseDir, c.ID)
		newDir := filepath.Join(baseDir, newDirName)
		if err := os.Rename(oldDir, newDir); err != nil {
			return err
		}
	}
	return nil
}
