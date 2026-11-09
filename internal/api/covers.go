package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"

	"github.com/JeremiahM37/librarr/internal/metadata"
)

const (
	maxCoverLookupBody  = 64 << 10
	maxCoverLookupItems = 20
	maxCoverLookupText  = 300 // bytes, for a title or author
	maxCoverLookupID    = 64  // bytes
)

type coverLookupItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Author    string `json:"author"`
	MediaType string `json:"media_type"`
}

// handleCoverLookup resolves cover URLs for a batch of search results. Misses,
// failures and non-book items are left out of the response.
func (s *Server) handleCoverLookup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCoverLookupBody)
	var req struct {
		Items []coverLookupItem `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body", nil)
		return
	}
	if len(req.Items) > maxCoverLookupItems {
		writeError(w, http.StatusBadRequest, "Too many items (max 20)", nil)
		return
	}
	for _, it := range req.Items {
		if len(it.ID) > maxCoverLookupID || len(it.Title) > maxCoverLookupText || len(it.Author) > maxCoverLookupText {
			writeError(w, http.StatusBadRequest, "Item field too long", nil)
			return
		}
	}

	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		result = map[string]string{}
	)
	for _, it := range req.Items {
		if it.MediaType != "ebook" && it.MediaType != "audiobook" {
			continue
		}
		wg.Add(1)
		go func(it coverLookupItem) {
			defer wg.Done()
			url, err := s.coverResolver.Resolve(r.Context(), metadata.CoverKindBook, it.Title, it.Author)
			if err != nil {
				slog.Debug("cover lookup failed", "error", err)
				return
			}
			if url == "" {
				return
			}
			mu.Lock()
			result[it.ID] = url
			mu.Unlock()
		}(it)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]interface{}{"covers": result})
}

// handleLibraryCover serves the saved cover of a library item.
func (s *Server) handleLibraryCover(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	item, err := s.db.GetItem(id)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			slog.Error("cover item lookup failed", "id", id, "error", err)
			http.Error(w, "Internal error", http.StatusInternalServerError)
			return
		}
		http.NotFound(w, r)
		return
	}
	if item.CoverFile == "" {
		http.NotFound(w, r)
		return
	}
	f, contentType, err := s.coverStore.Open(item.CoverFile)
	if err != nil {
		slog.Debug("cover file unavailable", "id", id, "error", err)
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	io.Copy(w, f)
}
