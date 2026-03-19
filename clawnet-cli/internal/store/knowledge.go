package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// KnowledgeEntry represents a knowledge item.
type KnowledgeEntry struct {
	ID          string   `json:"id"`
	AuthorID    string   `json:"author_id"`
	AuthorName  string   `json:"author_name"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Domains     []string `json:"domains"`
	Upvotes     int      `json:"upvotes"`
	Flags       int      `json:"flags"`
	CreatedAt   string   `json:"created_at"`
	Type        string   `json:"type,omitempty"`         // doc | task-insight | network-insight | agent-insight
	Source      string   `json:"source,omitempty"`       // context-hub | p2p | community | local
	ContentHash string   `json:"content_hash,omitempty"` // SHA-256 of body for dedup
}

// KnowledgeReply represents a reply to a knowledge entry.
type KnowledgeReply struct {
	ID          string `json:"id"`
	KnowledgeID string `json:"knowledge_id"`
	AuthorID    string `json:"author_id"`
	AuthorName  string `json:"author_name"`
	Body        string `json:"body"`
	CreatedAt   string `json:"created_at"`
}

// InsertKnowledge upserts a knowledge entry.
func (s *Store) InsertKnowledge(e *KnowledgeEntry) error {
	domains, _ := json.Marshal(e.Domains)
	if e.ContentHash == "" {
		e.ContentHash = hashContent(e.Body)
	}
	_, err := s.DB.Exec(
		`INSERT OR IGNORE INTO knowledge (id, author_id, author_name, title, body, domains, upvotes, flags, created_at, type, source, source_path, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.AuthorID, e.AuthorName, e.Title, e.Body, string(domains), e.Upvotes, e.Flags, e.CreatedAt,
		coalesce(e.Type, "doc"), e.Source, "", e.ContentHash,
	)
	return err
}

// GetKnowledge retrieves a single knowledge entry by ID.
func (s *Store) GetKnowledge(id string) (*KnowledgeEntry, error) {
	row := s.DB.QueryRow(
		`SELECT id, author_id, author_name, title, body, domains, upvotes, flags, created_at,
		        COALESCE(type,'doc'), COALESCE(source,''), COALESCE(content_hash,'')
		 FROM knowledge WHERE id = ?`, id,
	)
	return scanKnowledge(row)
}

// ListKnowledge returns knowledge entries, optionally filtered by domain.
func (s *Store) ListKnowledge(domain string, limit, offset int) ([]*KnowledgeEntry, error) {
	var rows *sql.Rows
	var err error
	if domain != "" {
		rows, err = s.DB.Query(
			`SELECT id, author_id, author_name, title, body, domains, upvotes, flags, created_at,
			        COALESCE(type,'doc'), COALESCE(source,''), COALESCE(content_hash,'')
			 FROM knowledge WHERE domains LIKE ? ORDER BY created_at DESC LIMIT ? OFFSET ?`,
			fmt.Sprintf("%%%s%%", likeSanitize(domain)), limit, offset,
		)
	} else {
		rows, err = s.DB.Query(
			`SELECT id, author_id, author_name, title, body, domains, upvotes, flags, created_at,
			        COALESCE(type,'doc'), COALESCE(source,''), COALESCE(content_hash,'')
			 FROM knowledge ORDER BY created_at DESC LIMIT ? OFFSET ?`,
			limit, offset,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKnowledgeRows(rows)
}

// SearchKnowledge performs full-text search using FTS5 when available,
// falling back to LIKE-based search on platforms without fts5 support.
func (s *Store) SearchKnowledge(query string, limit int) ([]*KnowledgeEntry, error) {
	if s.HasFTS5 {
		rows, err := s.DB.Query(
			`SELECT k.id, k.author_id, k.author_name, k.title, k.body, k.domains, k.upvotes, k.flags, k.created_at,
			        COALESCE(k.type,'doc'), COALESCE(k.source,''), COALESCE(k.content_hash,'')
			 FROM knowledge k
			 JOIN knowledge_fts f ON k.rowid = f.rowid
			 WHERE knowledge_fts MATCH ?
			 ORDER BY rank LIMIT ?`,
			query, limit,
		)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanKnowledgeRows(rows)
	}

	// Fallback: LIKE-based search (no fts5 module)
	pattern := "%" + likeSanitize(query) + "%"
	rows, err := s.DB.Query(
		`SELECT id, author_id, author_name, title, body, domains, upvotes, flags, created_at,
		        COALESCE(type,'doc'), COALESCE(source,''), COALESCE(content_hash,'')
		 FROM knowledge
		 WHERE title LIKE ? OR body LIKE ? OR domains LIKE ?
		 ORDER BY created_at DESC LIMIT ?`,
		pattern, pattern, pattern, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKnowledgeRows(rows)
}

// likeSanitize escapes LIKE wildcards in user input.
func likeSanitize(s string) string {
	r := strings.NewReplacer("%", "\\%", "_", "\\_")
	return r.Replace(s)
}

// ReactKnowledge records a reaction (upvote/flag) and updates counters.
func (s *Store) ReactKnowledge(knowledgeID, peerID, reaction string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Upsert reaction
	_, err = tx.Exec(
		`INSERT INTO knowledge_reactions (knowledge_id, peer_id, reaction)
		 VALUES (?, ?, ?)
		 ON CONFLICT(knowledge_id, peer_id) DO UPDATE SET reaction = excluded.reaction`,
		knowledgeID, peerID, reaction,
	)
	if err != nil {
		return err
	}

	// Recount
	_, err = tx.Exec(
		`UPDATE knowledge SET
			upvotes = (SELECT COUNT(*) FROM knowledge_reactions WHERE knowledge_id = ? AND reaction = 'upvote'),
			flags   = (SELECT COUNT(*) FROM knowledge_reactions WHERE knowledge_id = ? AND reaction = 'flag')
		 WHERE id = ?`,
		knowledgeID, knowledgeID, knowledgeID,
	)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// InsertReply adds a reply to a knowledge entry.
func (s *Store) InsertReply(r *KnowledgeReply) error {
	_, err := s.DB.Exec(
		`INSERT OR IGNORE INTO knowledge_replies (id, knowledge_id, author_id, author_name, body, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.KnowledgeID, r.AuthorID, r.AuthorName, r.Body, r.CreatedAt,
	)
	return err
}

// ListReplies returns replies for a knowledge entry.
func (s *Store) ListReplies(knowledgeID string, limit int) ([]*KnowledgeReply, error) {
	rows, err := s.DB.Query(
		`SELECT id, knowledge_id, author_id, author_name, body, created_at
		 FROM knowledge_replies WHERE knowledge_id = ? ORDER BY created_at ASC LIMIT ?`,
		knowledgeID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var replies []*KnowledgeReply
	for rows.Next() {
		r := &KnowledgeReply{}
		if err := rows.Scan(&r.ID, &r.KnowledgeID, &r.AuthorID, &r.AuthorName, &r.Body, &r.CreatedAt); err != nil {
			return nil, err
		}
		replies = append(replies, r)
	}
	return replies, rows.Err()
}

// scanner helpers

type scannable interface {
	Scan(dest ...any) error
}

func scanKnowledge(row scannable) (*KnowledgeEntry, error) {
	e := &KnowledgeEntry{}
	var domainsJSON string
	var contentHash sql.NullString
	err := row.Scan(&e.ID, &e.AuthorID, &e.AuthorName, &e.Title, &e.Body, &domainsJSON, &e.Upvotes, &e.Flags, &e.CreatedAt, &e.Type, &e.Source, &contentHash)
	if err != nil {
		return nil, err
	}
	json.Unmarshal([]byte(domainsJSON), &e.Domains)
	if e.Domains == nil {
		e.Domains = []string{}
	}
	if contentHash.Valid {
		e.ContentHash = contentHash.String
	}
	return e, nil
}

func scanKnowledgeRows(rows *sql.Rows) ([]*KnowledgeEntry, error) {
	var entries []*KnowledgeEntry
	for rows.Next() {
		e := &KnowledgeEntry{}
		var domainsJSON string
		var contentHash sql.NullString
		if err := rows.Scan(&e.ID, &e.AuthorID, &e.AuthorName, &e.Title, &e.Body, &domainsJSON, &e.Upvotes, &e.Flags, &e.CreatedAt, &e.Type, &e.Source, &contentHash); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(domainsJSON), &e.Domains)
		if e.Domains == nil {
			e.Domains = []string{}
		}
		if contentHash.Valid {
			e.ContentHash = contentHash.String
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListKnowledgeSince returns knowledge entries created after the given RFC3339 timestamp.
func (s *Store) ListKnowledgeSince(since string, limit int) ([]*KnowledgeEntry, error) {
	rows, err := s.DB.Query(
		`SELECT id, author_id, author_name, title, body, domains, upvotes, flags, created_at,
		        COALESCE(type,'doc'), COALESCE(source,''), COALESCE(content_hash,'')
		 FROM knowledge WHERE created_at > ? ORDER BY created_at ASC LIMIT ?`,
		since, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanKnowledgeRows(rows)
}

// LatestKnowledgeTime returns the created_at of the most recent knowledge entry.
func (s *Store) LatestKnowledgeTime() string {
	var t sql.NullString
	s.DB.QueryRow(`SELECT MAX(created_at) FROM knowledge`).Scan(&t)
	if t.Valid {
		return t.String
	}
	return ""
}

// EscapeFTS5 escapes a user query for safe FTS5 matching.
func EscapeFTS5(q string) string {
	// Wrap each word in double quotes to avoid FTS5 syntax issues
	words := strings.Fields(q)
	for i, w := range words {
		words[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	return strings.Join(words, " ")
}

// UpsertSyncedKnowledge inserts or updates a knowledge entry from an external sync source.
// Source/source_path are always overwritten on sync; user edits don't lose source attribution.
func (s *Store) UpsertSyncedKnowledge(e *KnowledgeEntry, sourcePath string) (created bool, err error) {
	domains, _ := json.Marshal(e.Domains)
	ktype := coalesce(e.Type, "doc")
	ch := e.ContentHash
	if ch == "" {
		ch = hashContent(e.Body)
	}

	res, err := s.DB.Exec(
		`INSERT INTO knowledge (id, author_id, author_name, title, body, domains, upvotes, flags, created_at, type, source, source_path, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 0, datetime('now'), ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			body = excluded.body,
			domains = excluded.domains,
			type = excluded.type,
			source = excluded.source,
			source_path = excluded.source_path,
			content_hash = excluded.content_hash`,
		e.ID, e.AuthorID, e.AuthorName, e.Title, e.Body, string(domains),
		ktype, e.Source, sourcePath, ch,
	)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	// ON CONFLICT DO UPDATE always reports 1 row affected,
	// so we check if the entry already existed.
	var count int
	s.DB.QueryRow(`SELECT COUNT(*) FROM knowledge WHERE id = ? AND created_at < datetime('now', '-1 second')`, e.ID).Scan(&count)
	return count == 0 && affected > 0, nil
}

// BulkUpsertSyncedKnowledge inserts/updates a batch of entries in one transaction.
// Returns (created, updated, errors) counts.
func (s *Store) BulkUpsertSyncedKnowledge(entries []*KnowledgeEntry, sourcePaths []string) (created, updated, errs int) {
	if len(entries) == 0 {
		return 0, 0, 0
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, 0, len(entries)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO knowledge (id, author_id, author_name, title, body, domains, upvotes, flags, created_at, type, source, source_path, content_hash)
		 VALUES (?, ?, ?, ?, ?, ?, 0, 0, datetime('now'), ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			body = excluded.body,
			domains = excluded.domains,
			type = excluded.type,
			source = excluded.source,
			source_path = excluded.source_path,
			content_hash = excluded.content_hash`)
	if err != nil {
		return 0, 0, len(entries)
	}
	defer stmt.Close()

	for i, e := range entries {
		domains, _ := json.Marshal(e.Domains)
		ktype := coalesce(e.Type, "doc")
		ch := e.ContentHash
		if ch == "" {
			ch = hashContent(e.Body)
		}
		sp := ""
		if i < len(sourcePaths) {
			sp = sourcePaths[i]
		}
		_, err := stmt.Exec(
			e.ID, e.AuthorID, e.AuthorName, e.Title, e.Body, string(domains),
			ktype, e.Source, sp, ch,
		)
		if err != nil {
			errs++
		} else {
			// Simplified: treat as created (exact create/update distinction is expensive per-row)
			created++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, len(entries)
	}
	return created, updated, errs
}

// CountKnowledgeBySource returns the count of knowledge entries from a given source.
func (s *Store) CountKnowledgeBySource(source string) int {
	var count int
	s.DB.QueryRow(`SELECT COUNT(*) FROM knowledge WHERE source = ?`, source).Scan(&count)
	return count
}

// SourceIcon returns the emoji icon for a knowledge source.
func SourceIcon(source string) string {
	switch source {
	case "context-hub":
		return "📚"
	case "p2p":
		return "🧠"
	case "community":
		return "🌐"
	default:
		return "📄"
	}
}

func coalesce(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// hashContent returns a SHA-256 hex digest of the content body.
func hashContent(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

// HashContent is the exported version of hashContent.
func HashContent(body string) string { return hashContent(body) }

// SearchKnowledgeDedup performs full-text search and deduplicates by content_hash.
// When multiple entries share the same hash, it keeps the one with the most upvotes.
func (s *Store) SearchKnowledgeDedup(query string, limit int) ([]*KnowledgeEntry, error) {
	// Over-fetch to account for dedup shrinkage
	raw, err := s.SearchKnowledge(query, limit*3)
	if err != nil {
		return nil, err
	}
	return dedup(raw, limit), nil
}

// dedup removes duplicate entries by content_hash, keeping the highest-upvoted.
func dedup(entries []*KnowledgeEntry, limit int) []*KnowledgeEntry {
	seen := map[string]int{} // content_hash → index in result
	var result []*KnowledgeEntry
	for _, e := range entries {
		h := e.ContentHash
		if h == "" {
			// No hash — always include
			result = append(result, e)
			if len(result) >= limit {
				break
			}
			continue
		}
		if idx, ok := seen[h]; ok {
			// Keep the one with more upvotes
			if e.Upvotes > result[idx].Upvotes {
				result[idx] = e
			}
			continue
		}
		seen[h] = len(result)
		result = append(result, e)
		if len(result) >= limit {
			break
		}
	}
	return result
}
