package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"strings"
	"time"
)

// Annotation is a local-only note attached to a knowledge entry.
type Annotation struct {
	ID          string `json:"id"`
	KnowledgeID string `json:"knowledge_id"`
	Note        string `json:"note"`
	CreatedAt   string `json:"created_at"`
}

// InsertAnnotation adds a local annotation to a knowledge entry.
func (s *Store) InsertAnnotation(knowledgeID, note string) error {
	id := randomID()
	_, err := s.DB.Exec(
		`INSERT INTO knowledge_annotations (id, knowledge_id, note, created_at)
		 VALUES (?, ?, ?, ?)`,
		id, knowledgeID, note, time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	)
	return err
}

// ListAnnotations returns annotations for a specific knowledge entry.
func (s *Store) ListAnnotations(knowledgeID string) ([]*Annotation, error) {
	rows, err := s.DB.Query(
		`SELECT id, knowledge_id, note, created_at
		 FROM knowledge_annotations WHERE knowledge_id = ? ORDER BY created_at ASC`,
		knowledgeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAnnotations(rows)
}

// ListAllAnnotations returns all annotations across all knowledge entries.
func (s *Store) ListAllAnnotations() ([]*Annotation, error) {
	rows, err := s.DB.Query(
		`SELECT id, knowledge_id, note, created_at
		 FROM knowledge_annotations ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAnnotations(rows)
}

// ClearAnnotations removes all annotations for a knowledge entry.
func (s *Store) ClearAnnotations(knowledgeID string) error {
	_, err := s.DB.Exec(`DELETE FROM knowledge_annotations WHERE knowledge_id = ?`, knowledgeID)
	return err
}

// FindKnowledgeBySourcePath finds knowledge entries whose source_path matches the query.
// Used for `clawnet get openai/chat` → match source_path containing those path segments.
func (s *Store) FindKnowledgeBySourcePath(pathQuery string, lang string, limit int) ([]*KnowledgeEntry, error) {
	// Split query by "/" and match each segment independently:
	// "openai/chat" → %openai%chat%  (matches content/openai/docs/chat/python/DOC.md)
	segments := strings.Split(pathQuery, "/")
	pattern := "%"
	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg != "" {
			pattern += likeSanitize(seg) + "%"
		}
	}
	if lang != "" {
		langDir := expandLang(lang)
		pattern += langDir + "%"
	}

	rows, err := s.DB.Query(
		`SELECT id, author_id, author_name, title, body, domains, upvotes, flags, created_at,
		        COALESCE(type,'doc'), COALESCE(source,''), COALESCE(content_hash,'')
		 FROM knowledge WHERE source_path LIKE ? ORDER BY created_at DESC LIMIT ?`,
		pattern, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKnowledgeRows(rows)
}

// expandLang maps short language codes to Context Hub directory names.
func expandLang(lang string) string {
	switch lang {
	case "py", "python":
		return "python"
	case "js", "javascript":
		return "javascript"
	case "ts", "typescript":
		return "typescript"
	case "go", "golang":
		return "go"
	case "rs", "rust":
		return "rust"
	case "java":
		return "java"
	case "rb", "ruby":
		return "ruby"
	case "cs", "csharp":
		return "csharp"
	default:
		return lang
	}
}

// ExpandLangPublic is the exported version for use by daemon handlers.
func ExpandLangPublic(lang string) string { return expandLang(lang) }

func scanAnnotations(rows *sql.Rows) ([]*Annotation, error) {
	var annotations []*Annotation
	for rows.Next() {
		a := &Annotation{}
		if err := rows.Scan(&a.ID, &a.KnowledgeID, &a.Note, &a.CreatedAt); err != nil {
			return nil, err
		}
		annotations = append(annotations, a)
	}
	return annotations, rows.Err()
}

func randomID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
