package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ChatChatTech/ClawNet/clawnet-cli/internal/knowledge"
	"github.com/ChatChatTech/ClawNet/clawnet-cli/internal/store"
)

// handleKnowledgeSync handles POST /api/knowledge/sync — sync from external source.
// Supports two modes:
//   - GitHub API:  {"source": "github:owner/repo/path", "token": "..."}
//   - Local dir:   {"local": "/data/projs/context-hub/content"}
func (d *Daemon) handleKnowledgeSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Source string `json:"source"` // github:owner/repo/path
		Local  string `json:"local"`  // local filesystem path to content dir
		DryRun bool   `json:"dry_run"`
		Token  string `json:"token"` // optional GitHub API token
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	// Local filesystem sync
	if req.Local != "" {
		d.syncFromLocal(w, req.Local, req.DryRun)
		return
	}

	// GitHub API sync (existing path)
	if req.Source == "" {
		req.Source = knowledge.DefaultContextHubURI
	}

	owner, repo, dirPath, err := knowledge.ParseGitHubURI(req.Source)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error(),
			withSuggestion("Use format: github:owner/repo/path or {\"local\":\"/path/to/content\"}"),
			withHelp("GET /api/endpoints"))
		return
	}

	syncer := knowledge.NewGitHubSyncer(req.Token)
	files, err := syncer.ListMarkdownFiles(owner, repo, dirPath)
	if err != nil {
		apiError(w, http.StatusBadGateway, fmt.Sprintf("GitHub API error: %v", err),
			withSuggestion("Check repo access or provide a --token for higher rate limits"))
		return
	}

	sourceLabel := knowledge.ContextHubSource
	if !(owner == "andrewyng" && (repo == "ContextHub" || repo == "context-hub")) {
		sourceLabel = fmt.Sprintf("github:%s/%s", owner, repo)
	}

	result := knowledge.SyncResult{Source: sourceLabel, Total: len(files)}

	for _, f := range files {
		if req.DryRun {
			result.Created++
			continue
		}

		content, err := syncer.FetchFileContent(f.DownloadURL)
		if err != nil {
			result.Errors++
			continue
		}

		doc := knowledge.ParseDocument(f.Path, content, sourceLabel)
		docID := knowledge.KnowledgeID(sourceLabel, f.Path)

		entry := &store.KnowledgeEntry{
			ID:          docID,
			AuthorID:    "system_sync",
			AuthorName:  sourceLabel,
			Title:       doc.Title,
			Body:        doc.Body,
			Domains:     doc.Domains,
			Type:        "doc",
			Source:      sourceLabel,
			ContentHash: store.HashContent(doc.Body),
			CreatedAt:   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		}

		created, err := d.Store.UpsertSyncedKnowledge(entry, f.Path)
		if err != nil {
			result.Errors++
			continue
		}

		if created {
			result.Created++
		} else {
			result.Updated++
		}
	}

	result.Skipped = result.Total - result.Created - result.Updated - result.Errors

	// Record sync event
	if !req.DryRun && result.Created > 0 {
		d.RecordEvent("knowledge_sync", "system",
			sourceLabel,
			fmt.Sprintf("Synced %d docs (%d new, %d updated) from %s",
				result.Total, result.Created, result.Updated, sourceLabel))

		// Check knowledge_sharer achievement
		d.CheckAchievements()
	}

	writeJSON(w, result)
}

// syncFromLocal imports knowledge entries from a local filesystem directory.
// Path format: contentDir/<package>/docs/<topic>/<lang>/DOC.md
// Uses SSE streaming for real-time progress, concurrent workers, and batched DB writes.
func (d *Daemon) syncFromLocal(w http.ResponseWriter, contentDir string, dryRun bool) {
	files, err := knowledge.ListLocalMarkdownFiles(contentDir)
	if err != nil {
		apiError(w, http.StatusBadRequest, fmt.Sprintf("cannot read local dir: %v", err),
			withSuggestion("Ensure the path exists: "+contentDir))
		return
	}

	sourceLabel := knowledge.ContextHubSource
	total := len(files)

	// Check if client wants SSE streaming (Accept: text/event-stream)
	wantSSE := false
	for _, a := range []string{"text/event-stream", "text/event"} {
		if headerContains(w, a) {
			wantSSE = true
			break
		}
	}
	_ = wantSSE // we always stream if possible

	// Set up SSE
	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	sendSSE := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		if canFlush {
			flusher.Flush()
		}
	}

	// Send initial info
	sendSSE("info", fmt.Sprintf(`{"total":%d,"source":"%s (local)"}`, total, sourceLabel))

	if dryRun {
		sendSSE("done", fmt.Sprintf(`{"source":"%s (local)","total":%d,"created":%d,"updated":0,"skipped":0,"errors":0}`,
			sourceLabel, total, total))
		return
	}

	// ── Concurrent pipeline ──
	const numWorkers = 16
	const batchSize = 100

	type parsed struct {
		entry      *store.KnowledgeEntry
		sourcePath string
	}

	parsedCh := make(chan parsed, numWorkers*4)
	var wg sync.WaitGroup
	var progress int64

	// Workers: read + parse files
	fileCh := make(chan knowledge.LocalFile, numWorkers*2)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range fileCh {
				content, err := knowledge.ReadLocalFile(f.AbsPath)
				if err != nil {
					atomic.AddInt64(&progress, 1)
					continue
				}
				sourcePath := "content/" + f.Path
				doc := knowledge.ParseDocument(sourcePath, content, sourceLabel)
				docID := knowledge.KnowledgeID(sourceLabel, sourcePath)

				entry := &store.KnowledgeEntry{
					ID:          docID,
					AuthorID:    "system_sync",
					AuthorName:  sourceLabel,
					Title:       doc.Title,
					Body:        doc.Body,
					Domains:     doc.Domains,
					Type:        "doc",
					Source:      sourceLabel,
					ContentHash: store.HashContent(doc.Body),
				}
				parsedCh <- parsed{entry: entry, sourcePath: sourcePath}
			}
		}()
	}

	// Feed files to workers
	go func() {
		for _, f := range files {
			fileCh <- f
		}
		close(fileCh)
		wg.Wait()
		close(parsedCh)
	}()

	// Batch collector: accumulate parsed entries and write in batches
	var (
		totalCreated int
		totalUpdated int
		totalErrors  int
		batch        []*store.KnowledgeEntry
		batchPaths   []string
		lastReport   time.Time
	)

	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		c, u, e := d.Store.BulkUpsertSyncedKnowledge(batch, batchPaths)
		totalCreated += c
		totalUpdated += u
		totalErrors += e
		atomic.AddInt64(&progress, int64(len(batch)))
		batch = batch[:0]
		batchPaths = batchPaths[:0]
	}

	for p := range parsedCh {
		batch = append(batch, p.entry)
		batchPaths = append(batchPaths, p.sourcePath)

		if len(batch) >= batchSize {
			flushBatch()
		}

		// Send progress at most every 200ms
		now := time.Now()
		if now.Sub(lastReport) > 200*time.Millisecond {
			cur := int(atomic.LoadInt64(&progress)) + len(batch)
			sendSSE("progress", fmt.Sprintf(`{"done":%d,"total":%d}`, cur, total))
			lastReport = now
		}
	}

	// Final batch
	flushBatch()

	result := knowledge.SyncResult{
		Source:  sourceLabel + " (local)",
		Total:   total,
		Created: totalCreated,
		Updated: totalUpdated,
		Errors:  totalErrors,
	}
	result.Skipped = result.Total - result.Created - result.Updated - result.Errors

	if result.Created > 0 {
		d.RecordEvent("knowledge_sync", "system",
			sourceLabel,
			fmt.Sprintf("Local sync: %d docs (%d new, %d updated) from %s",
				result.Total, result.Created, result.Updated, contentDir))
		d.CheckAchievements()
	}

	resultJSON, _ := json.Marshal(result)
	sendSSE("done", string(resultJSON))
}

// headerContains checks Accept header (unused but kept for clarity).
func headerContains(_ http.ResponseWriter, _ string) bool {
	return false
}
