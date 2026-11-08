package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	maxSearchQueryBytes = 1024
	maxSearchResults    = 250
	searchReaderBytes   = 256 << 10
	searchSnippetBytes  = 220
)

type documentSearchMatch struct {
	Offset  int64  `json:"offset"`
	Line    int64  `json:"line,omitempty"`
	Snippet string `json:"snippet"`
}

type documentSearchResponse struct {
	Instance     string                `json:"instance"`
	Key          string                `json:"key"`
	Query        string                `json:"query"`
	Matches      []documentSearchMatch `json:"matches"`
	Total        int64                 `json:"total"`
	Truncated    bool                  `json:"truncated,omitempty"`
	BytesScanned int64                 `json:"bytesScanned"`
}

func (a *application) handleDocumentSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	instance, err := a.instanceFromRequest(r, "")
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := requirePermission(instance, permissionRead); err != nil {
		writeAPIError(w, err)
		return
	}
	key := cleanRelativeKey(r.URL.Query().Get("key"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if key == "" || query == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_search", Message: "key and q are required"})
		return
	}
	if len(query) > maxSearchQueryBytes {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "search_query_too_long", Message: fmt.Sprintf("search queries are limited to %d bytes", maxSearchQueryBytes)})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}
	response, err := instance.Get(r.Context(), instance.fullKey(key), nil)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer response.Body.Close()
	if !isSuccessfulObjectReadStatus(response.StatusCode) {
		writeAPIError(w, fmt.Errorf("search object: HTTP %d", response.StatusCode))
		return
	}
	result, err := searchDocumentStream(response.Body, []byte(query))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	result.Instance = instance.cfg.ID
	result.Key = key
	result.Query = query
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

func searchDocumentStream(source io.Reader, query []byte) (documentSearchResponse, error) {
	result := documentSearchResponse{Matches: make([]documentSearchMatch, 0)}
	if len(query) == 0 {
		return result, nil
	}
	lowerQuery := bytes.ToLower(query)
	reader := bufio.NewReaderSize(source, searchReaderBytes)
	lineNumber := int64(1)
	fragmentStartOffset := int64(0)
	var carry []byte
	lastOffset := int64(-1)

	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			window := make([]byte, 0, len(carry)+len(fragment))
			window = append(window, carry...)
			window = append(window, fragment...)
			windowStart := fragmentStartOffset - int64(len(carry))
			lowerWindow := bytes.ToLower(window)
			searchAt := 0
			for searchAt <= len(lowerWindow)-len(lowerQuery) {
				index := bytes.Index(lowerWindow[searchAt:], lowerQuery)
				if index < 0 {
					break
				}
				index += searchAt
				offset := windowStart + int64(index)
				if offset > lastOffset {
					result.Total++
					lastOffset = offset
					if len(result.Matches) < maxSearchResults {
						result.Matches = append(result.Matches, documentSearchMatch{
							Offset:  offset,
							Line:    lineNumber,
							Snippet: searchSnippet(window, index, len(query)),
						})
					}
				}
				searchAt = index + maxInt(1, len(lowerQuery))
			}
			fragmentStartOffset += int64(len(fragment))
			result.BytesScanned += int64(len(fragment))
			keep := minInt(len(window), maxInt(0, len(query)-1))
			carry = append(carry[:0], window[len(window)-keep:]...)
		}

		if readErr == nil {
			lineNumber++
			carry = carry[:0]
			continue
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		return documentSearchResponse{}, readErr
	}
	result.Truncated = result.Total > int64(len(result.Matches))
	return result, nil
}

func searchSnippet(window []byte, index, queryLength int) string {
	start := maxInt(0, index-searchSnippetBytes/2)
	end := minInt(len(window), index+queryLength+searchSnippetBytes/2)
	for start > 0 && !utf8.RuneStart(window[start]) {
		start--
	}
	for end < len(window) && !utf8.RuneStart(window[end]) {
		end++
	}
	value := strings.ToValidUTF8(string(window[start:end]), "�")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ↵ ")
	value = strings.Join(strings.Fields(value), " ")
	if start > 0 {
		value = "…" + value
	}
	if end < len(window) {
		value += "…"
	}
	return value
}
