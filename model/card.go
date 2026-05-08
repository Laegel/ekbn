package model

import (
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

type Card struct {
	Title      string    `yaml:"title" json:"title"`
	Author     string    `yaml:"author" json:"author"`
	Created    string    `yaml:"created" json:"created"`
	Updated    string    `yaml:"updated" json:"updated"`
	Priority   int       `yaml:"priority" json:"priority"`
	Role       string    `yaml:"role" json:"role"`
	Blocked    bool      `yaml:"blocked" json:"blocked"`
	Categories []string  `yaml:"categories" json:"categories"`
	Comments   []Comment `yaml:"comments" json:"comments"`
	Content    string    `yaml:"-" json:"content"`
	Filename   string    `yaml:"-" json:"filename"`
	Column     string    `yaml:"-" json:"column"`
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

var DefaultColumns = []ColumnDef{
	{100, "todo", "To Do"},
	{200, "in-progress", "In Progress"},
	{250, "review", "Review"},
	{300, "done", "Done"},
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

func CreateColumn(baseDir string, slug string, name string) (string, int, error) {
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

func CreateCard(baseDir string, column string, title string, author string, content string, categories []string, priority int, role string, blocked bool) (string, error) {
	now := time.Now().Format("2006-01-02T15:04:05Z07:00")
	card := Card{
		Title:      title,
		Author:     author,
		Created:    now,
		Updated:    now,
		Priority:   priority,
		Role:       role,
		Blocked:    blocked,
		Categories: categories,
		Content:    content,
	}

	fmData, err := yaml.Marshal(&card)
	if err != nil {
		return "", err
	}

	fileContent := fmt.Sprintf("---\n%s---\n\n%s", string(fmData), content)

	filename := fmt.Sprintf("%s.md", slugify(title))
	path := filepath.Join(baseDir, column, filename)
	if err := os.WriteFile(path, []byte(fileContent), 0644); err != nil {
		return "", err
	}
	return filename, nil
}

func UpdateCard(baseDir string, column string, filename string, title string, content string, categories []string, priority int, role string, blocked bool) error {
	path := filepath.Join(baseDir, column, filename)
	card, err := ReadCard(path, column)
	if err != nil {
		return err
	}

	now := time.Now().Format("2006-01-02T15:04:05Z07:00")
	card.Title = title
	card.Content = content
	card.Categories = categories
	card.Priority = priority
	card.Role = role
	card.Blocked = blocked
	card.Updated = now

	fmData, err := yaml.Marshal(&card)
	if err != nil {
		return err
	}

	fileContent := fmt.Sprintf("---\n%s---\n\n%s", string(fmData), content)
	return os.WriteFile(path, []byte(fileContent), 0644)
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

	fmData, err := yaml.Marshal(&card)
	if err != nil {
		return err
	}

	fileContent := fmt.Sprintf("---\n%s---\n\n%s", string(fmData), card.Content)
	return os.WriteFile(path, []byte(fileContent), 0644)
}

func DeleteCard(baseDir string, column string, filename string) error {
	path := filepath.Join(baseDir, column, filename)
	return os.Remove(path)
}

func MoveCard(baseDir string, fromColumn string, toColumn string, filename string) error {
	src := filepath.Join(baseDir, fromColumn, filename)
	dst := filepath.Join(baseDir, toColumn, filename)
	return os.Rename(src, dst)
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
