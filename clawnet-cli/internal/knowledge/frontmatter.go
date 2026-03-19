package knowledge

import (
	"bufio"
	"strings"
)

// Frontmatter holds parsed YAML frontmatter fields from a Context Hub doc.
type Frontmatter struct {
	Title       string
	Description string
	Tags        []string
	Version     string
	Languages   string            // e.g. "python", "javascript"
	Source      string            // e.g. "maintainer", "community"
	Extra       map[string]string // any other key: value pairs
}

// ParseFrontmatter extracts YAML frontmatter from markdown content.
// Returns the parsed frontmatter and the remaining body (after the closing ---).
// If no frontmatter is found, returns nil and the entire content as body.
func ParseFrontmatter(content string) (*Frontmatter, string) {
	scanner := bufio.NewScanner(strings.NewReader(content))

	// First line must be "---"
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return nil, content
	}

	fm := &Frontmatter{Extra: make(map[string]string)}
	var bodyStart int
	lineNum := 1 // already consumed the opening ---
	inTags := false
	inMetadata := false // track nested metadata: block

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()

		if strings.TrimSpace(line) == "---" {
			// End of frontmatter — collect remaining lines as body
			var bodyLines []string
			for scanner.Scan() {
				bodyLines = append(bodyLines, scanner.Text())
			}
			body := strings.Join(bodyLines, "\n")
			body = strings.TrimLeft(body, "\n")
			_ = bodyStart
			return fm, body
		}

		// Simple YAML parsing (key: value / - item)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			inTags = false
			continue
		}

		// Tag list items: "  - tag_name"
		if inTags && strings.HasPrefix(trimmed, "- ") {
			tag := strings.TrimSpace(trimmed[2:])
			if tag != "" {
				fm.Tags = append(fm.Tags, tag)
			}
			continue
		}

		// Detect indented lines inside metadata: block
		if inMetadata && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			inTags = false
			idx := strings.Index(trimmed, ":")
			if idx < 0 {
				continue
			}
			key := strings.TrimSpace(trimmed[:idx])
			val := strings.TrimSpace(trimmed[idx+1:])
			val = stripQuotes(val)

			switch strings.ToLower(key) {
			case "languages":
				fm.Languages = val
			case "versions":
				fm.Version = val
			case "source":
				fm.Source = val
			case "tags":
				if val == "" {
					inTags = true
				} else {
					fm.Tags = parseTags(val)
				}
			default:
				fm.Extra["metadata."+key] = val
			}
			continue
		}

		inTags = false
		inMetadata = false

		// key: value pairs
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])

		// Strip quotes
		val = stripQuotes(val)

		switch strings.ToLower(key) {
		case "title", "name":
			fm.Title = val
		case "description":
			fm.Description = val
		case "version":
			fm.Version = val
		case "languages":
			fm.Languages = val
		case "metadata":
			inMetadata = true
		case "tags":
			if val == "" {
				inTags = true
			} else {
				// Inline tags: tags: [a, b, c] or tags: a, b, c
				fm.Tags = parseTags(val)
			}
		default:
			fm.Extra[key] = val
		}
	}

	// No closing --- found — not valid frontmatter
	return nil, content
}

// InferMetadata generates metadata from the file path when frontmatter is absent.
func InferMetadata(filePath string) Frontmatter {
	fm := Frontmatter{Extra: make(map[string]string)}

	// Extract filename from path
	parts := strings.Split(filePath, "/")
	name := parts[len(parts)-1]

	// Strip .md extension
	name = strings.TrimSuffix(name, ".md")
	name = strings.TrimSuffix(name, ".mdx")

	// Convert kebab-case/snake_case to title
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	fm.Title = strings.Title(name)

	// Infer domain tags from directory path
	for _, part := range parts {
		if part == "" || part == "." || part == "content" {
			continue
		}
		lower := strings.ToLower(part)
		if lower != name && !strings.HasSuffix(lower, ".md") {
			fm.Tags = append(fm.Tags, lower)
		}
	}

	return fm
}

func stripQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func parseTags(val string) []string {
	// Handle [a, b, c] or a, b, c
	val = strings.TrimPrefix(val, "[")
	val = strings.TrimSuffix(val, "]")
	parts := strings.Split(val, ",")
	var tags []string
	for _, p := range parts {
		t := strings.TrimSpace(p)
		t = stripQuotes(t)
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}
