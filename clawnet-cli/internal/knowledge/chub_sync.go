package knowledge

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// SyncSource defines a knowledge source to sync from.
type SyncSource struct {
	// github:user/repo/path format
	URI     string `json:"uri"`
	Enabled bool   `json:"enabled"`
}

// SyncResult contains statistics from a sync operation.
type SyncResult struct {
	Source  string `json:"source"`
	Total   int    `json:"total"`
	Created int    `json:"created"`
	Updated int    `json:"updated"`
	Skipped int    `json:"skipped"`
	Errors  int    `json:"errors"`
}

// GHFile represents a file entry from GitHub Contents API.
type GHFile struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Type        string `json:"type"` // "file" or "dir"
	DownloadURL string `json:"download_url"`
	SHA         string `json:"sha"`
	Size        int    `json:"size"`
}

// KnowledgeDoc is a parsed document ready for import.
type KnowledgeDoc struct {
	Title       string
	Body        string
	Domains     []string
	Tags        []string
	Source      string
	SourcePath  string
	Description string
	Version     string
	Languages   string // e.g. "python", "javascript"
}

// GitHubSyncer syncs knowledge from GitHub repositories.
type GitHubSyncer struct {
	HTTPClient *http.Client
	Token      string // optional GitHub API token for higher rate limits
}

// NewGitHubSyncer creates a syncer with sensible defaults.
func NewGitHubSyncer(token string) *GitHubSyncer {
	return &GitHubSyncer{
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Token:      token,
	}
}

// ParseGitHubURI parses "github:user/repo/path" into (owner, repo, dirPath).
func ParseGitHubURI(uri string) (owner, repo, dirPath string, err error) {
	s := strings.TrimPrefix(uri, "github:")
	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 {
		return "", "", "", fmt.Errorf("invalid GitHub URI: %q (expected github:owner/repo[/path])", uri)
	}
	owner = parts[0]
	repo = parts[1]
	if len(parts) == 3 {
		dirPath = parts[2]
	}
	return owner, repo, dirPath, nil
}

// ListMarkdownFiles lists all .md files in a GitHub repository directory (recursive).
func (gs *GitHubSyncer) ListMarkdownFiles(owner, repo, dirPath string) ([]GHFile, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s",
		owner, repo, dirPath)

	entries, err := gs.fetchContents(url)
	if err != nil {
		return nil, fmt.Errorf("list %s/%s/%s: %w", owner, repo, dirPath, err)
	}

	var mdFiles []GHFile
	for _, entry := range entries {
		switch entry.Type {
		case "file":
			lower := strings.ToLower(entry.Name)
			if !strings.HasSuffix(lower, ".md") {
				continue
			}
			// Skip non-content files
			if lower == "readme.md" || lower == "license.md" || lower == "changelog.md" || lower == "contributing.md" {
				continue
			}
			mdFiles = append(mdFiles, entry)
		case "dir":
			// Recurse into subdirectories
			subFiles, err := gs.ListMarkdownFiles(owner, repo, entry.Path)
			if err != nil {
				// Log and continue — don't fail entire sync for one subdir
				continue
			}
			mdFiles = append(mdFiles, subFiles...)
		}
	}
	return mdFiles, nil
}

// FetchFileContent downloads a file's content from its download URL.
func (gs *GitHubSyncer) FetchFileContent(downloadURL string) (string, error) {
	req, err := http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return "", err
	}
	if gs.Token != "" {
		req.Header.Set("Authorization", "token "+gs.Token)
	}
	req.Header.Set("Accept", "application/vnd.github.v3.raw")

	resp, err := gs.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: HTTP %d", downloadURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB max
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ParseDocument parses a markdown file into a KnowledgeDoc.
func ParseDocument(filePath, content, sourceLabel string) KnowledgeDoc {
	fm, body := ParseFrontmatter(content)

	doc := KnowledgeDoc{
		Body:       body,
		Source:     sourceLabel,
		SourcePath: filePath,
	}

	if fm != nil {
		doc.Title = fm.Title
		doc.Description = fm.Description
		doc.Tags = fm.Tags
		doc.Version = fm.Version
		doc.Languages = fm.Languages
		if len(fm.Tags) > 0 {
			doc.Domains = fm.Tags
		}
	}

	// Build a better title from Context Hub path structure:
	// content/<package>/docs/<topic>/<language>/DOC.md
	// → "package/topic (language)" when title is just a topic name
	if doc.Title != "" {
		parts := strings.Split(filePath, "/")
		// Try to extract package name from path
		for i, p := range parts {
			if p == "content" && i+1 < len(parts) {
				pkg := parts[i+1]
				// If title is just the topic name (e.g. "database", "chat"), prepend package
				if !strings.Contains(doc.Title, pkg) {
					doc.Title = pkg + "/" + doc.Title
				}
				break
			}
		}
		// Append language suffix if available
		if doc.Languages != "" {
			doc.Title = doc.Title + " (" + doc.Languages + ")"
		}
	}

	// Fallback: infer from file path
	if doc.Title == "" {
		inferred := InferMetadata(filePath)
		doc.Title = inferred.Title
		if len(doc.Domains) == 0 {
			doc.Domains = inferred.Tags
		}
	}

	// Clean up: ensure body isn't the entire content when title was inferred
	if doc.Body == "" {
		doc.Body = content
	}

	return doc
}

// KnowledgeID generates a deterministic ID for a synced knowledge entry.
// Uses source + path to ensure deduplication across syncs.
// For Context Hub paths like "content/openai/docs/chat/python/DOC.md",
// generates "chub-openai-chat-python" for human-readable IDs.
func KnowledgeID(source, filePath string) string {
	// Try to extract structured ID from Context Hub path pattern:
	// content/<package>/docs/<topic>/<language>/DOC.md
	parts := strings.Split(filePath, "/")
	var pkg, topic, lang string
	for i, p := range parts {
		if p == "content" && i+1 < len(parts) {
			pkg = parts[i+1]
			// Look for docs/<topic>/<lang>
			for j := i + 2; j < len(parts); j++ {
				if parts[j] == "docs" && j+1 < len(parts) {
					topic = parts[j+1]
					if j+2 < len(parts) && strings.ToLower(parts[j+2]) != "doc.md" {
						lang = strings.TrimSuffix(strings.ToLower(parts[j+2]), ".md")
						if lang == "doc" {
							lang = ""
						}
					}
				}
			}
			break
		}
	}

	if pkg != "" && topic != "" {
		id := "chub-" + pkg + "-" + topic
		if lang != "" {
			id += "-" + lang
		}
		return id
	}

	// Fallback: sanitize entire path
	sanitized := strings.ReplaceAll(filePath, "/", "-")
	sanitized = strings.ReplaceAll(sanitized, " ", "-")
	sanitized = strings.TrimSuffix(sanitized, ".md")
	return "chub-" + sanitized
}

// contextHubSource is the well-known source label for Context Hub.
const ContextHubSource = "context-hub"

// DefaultContextHubURI is the default sync target.
const DefaultContextHubURI = "github:andrewyng/context-hub/content"

// fetchContents calls the GitHub Contents API.
func (gs *GitHubSyncer) fetchContents(apiURL string) ([]GHFile, error) {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	if gs.Token != "" {
		req.Header.Set("Authorization", "token "+gs.Token)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := gs.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("GitHub API rate limit exceeded (use --token for 5000 req/h)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB max
	if err != nil {
		return nil, err
	}

	var entries []GHFile
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return entries, nil
}

// FileExtension returns the lowercase extension of a filename.
func fileExtension(name string) string {
	ext := path.Ext(name)
	return strings.ToLower(ext)
}

// ── Local filesystem syncer ──

// LocalFile represents a local markdown file ready for import.
type LocalFile struct {
	Path    string // relative path from content root, e.g. "openai/docs/chat/python/DOC.md"
	AbsPath string // absolute filesystem path
}

// ListLocalMarkdownFiles walks a local directory tree and returns all DOC.md files.
func ListLocalMarkdownFiles(contentDir string) ([]LocalFile, error) {
	var files []LocalFile
	err := walkDir(contentDir, contentDir, &files)
	return files, err
}

func walkDir(root, current string, files *[]LocalFile) error {
	entries, err := os.ReadDir(current)
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := filepath.Join(current, e.Name())
		if e.IsDir() {
			walkDir(root, full, files)
			continue
		}
		lower := strings.ToLower(e.Name())
		if !strings.HasSuffix(lower, ".md") {
			continue
		}
		// Skip non-content files
		if lower == "readme.md" || lower == "license.md" || lower == "changelog.md" || lower == "contributing.md" {
			continue
		}
		rel, _ := filepath.Rel(root, full)
		// Normalize to forward slashes for cross-platform path handling
		rel = filepath.ToSlash(rel)
		*files = append(*files, LocalFile{Path: rel, AbsPath: full})
	}
	return nil
}

// ReadLocalFile reads a local file's content.
func ReadLocalFile(absPath string) (string, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return "", err
	}
	if len(data) > 1<<20 { // 1MB limit
		data = data[:1<<20]
	}
	return string(data), nil
}
