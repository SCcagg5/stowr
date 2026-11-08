package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	jsonTextPageBytes       = 256 * 1024
	jsonTextPageLines       = 2000
	jsonTreeDefaultPageSize = 50
	jsonTreeMaxPageSize     = 200
	jsonTreePreviewBytes    = 1024
	jsonTreeKeyPreviewBytes = 2048
	jsonMaxNestingDepth     = 10000
)

type jsonTextCursor struct {
	Offset    int64         `json:"o"`
	Line      int64         `json:"l"`
	Continued bool          `json:"c,omitempty"`
	Stack     string        `json:"k,omitempty"`
	InString  bool          `json:"q,omitempty"`
	Escaped   bool          `json:"e,omitempty"`
	InScalar  bool          `json:"n,omitempty"`
	Pending   byte          `json:"p,omitempty"`
	Scope     string        `json:"s"`
	Version   objectVersion `json:"v,omitempty"`
}

type jsonTextPageResponse struct {
	Text          string `json:"text"`
	NextCursor    string `json:"nextCursor,omitempty"`
	Done          bool   `json:"done"`
	LineStart     int64  `json:"lineStart"`
	LineEnd       int64  `json:"lineEnd"`
	Continued     bool   `json:"continued,omitempty"`
	StartInString bool   `json:"startInString,omitempty"`
	StartEscaped  bool   `json:"startEscaped,omitempty"`
}

type jsonSummaryResponse struct {
	RawLines      int64                 `json:"rawLines"`
	RawPages      int64                 `json:"rawPages"`
	BeautifyLines int64                 `json:"beautifyLines"`
	BeautifyPages int64                 `json:"beautifyPages"`
	RawPage       *jsonTextPageResponse `json:"rawPage,omitempty"`
}

type jsonTreeNodeJSON struct {
	Label      string `json:"label,omitempty"`
	Type       string `json:"type"`
	Preview    string `json:"preview,omitempty"`
	Container  bool   `json:"container"`
	Start      int64  `json:"start"`
	End        int64  `json:"end,omitempty"`
	Count      int64  `json:"count,omitempty"`
	CountKnown bool   `json:"countKnown,omitempty"`
}

type jsonTreeCursor struct {
	Offset  int64         `json:"o"`
	Index   int64         `json:"i"`
	Start   int64         `json:"n"`
	Type    string        `json:"t"`
	Scope   string        `json:"s"`
	Version objectVersion `json:"v,omitempty"`
}

type jsonTreeResponse struct {
	Node      jsonTreeNodeJSON   `json:"node"`
	Children  []jsonTreeNodeJSON `json:"children"`
	Cursor    string             `json:"cursor,omitempty"`
	NextIndex int64              `json:"nextIndex,omitempty"`
	Done      bool               `json:"done"`
}

type jsonStreamReader struct {
	reader *bufio.Reader
	offset int64
}

func newJSONStreamReader(reader io.Reader, offset int64) *jsonStreamReader {
	return &jsonStreamReader{reader: bufio.NewReaderSize(reader, 64*1024), offset: offset}
}

func (r *jsonStreamReader) readByte() (byte, error) {
	value, err := r.reader.ReadByte()
	if err == nil {
		r.offset++
	}
	return value, err
}

func (r *jsonStreamReader) peekByte() (byte, error) {
	value, err := r.reader.Peek(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (r *jsonStreamReader) readRune() (rune, int, error) {
	value, size, err := r.reader.ReadRune()
	if err == nil {
		r.offset += int64(size)
	}
	return value, size, err
}

func (r *jsonStreamReader) skipWhitespace() error {
	for {
		value, err := r.peekByte()
		if err != nil {
			return err
		}
		switch value {
		case ' ', '\t', '\r', '\n':
			_, _ = r.readByte()
		default:
			return nil
		}
	}
}

func decodeJSONTreeCursor(raw, expectedScope string) (jsonTreeCursor, bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return jsonTreeCursor{}, false, nil
	}
	var cursor jsonTreeCursor
	if err := decodeSignedCursor(raw, &cursor); err != nil || cursor.Scope != expectedScope || cursor.Offset < 0 || cursor.Index < 0 || cursor.Start < 0 || (cursor.Type != "object" && cursor.Type != "array") {
		return jsonTreeCursor{}, false, apiError{Status: http.StatusBadRequest, Code: "invalid_cursor", Message: "invalid or expired JSON tree continuation cursor"}
	}
	return cursor, true, nil
}

func parseNonNegativeInt64(value string, fallback int64) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, apiError{Status: http.StatusBadRequest, Code: "invalid_offset", Message: "JSON byte offsets must be non-negative integers"}
	}
	return parsed, nil
}

func parsePositiveInt(value string, fallback, maximum int) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 || parsed > maximum {
		return 0, apiError{Status: http.StatusBadRequest, Code: "invalid_limit", Message: fmt.Sprintf("limit must be between 1 and %d", maximum)}
	}
	return parsed, nil
}

func (a *application) jsonObjectFromRequest(r *http.Request) (*storageInstance, string, string, error) {
	instance, err := a.instanceFromRequest(r, "")
	if err != nil {
		return nil, "", "", err
	}
	if err := requirePermission(instance, permissionRead); err != nil {
		return nil, "", "", err
	}
	key := cleanRelativeKey(r.URL.Query().Get("key"))
	if key == "" {
		return nil, "", "", apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "JSON object key cannot be empty"}
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		return nil, "", "", err
	}
	return instance, key, strings.TrimSpace(r.URL.Query().Get("etag")), nil
}

func openJSONObjectStream(ctx context.Context, instance *storageInstance, key string, offset int64, etag string, version objectVersion) (*objectWindowReader, error) {
	if version.empty() {
		version.ETag = etag
	}
	return newObjectWindowReaderVersion(ctx, instance, key, offset, version)
}

func encodeJSONCursor(cursor jsonTextCursor) (string, error) {
	return encodeSignedCursor(cursor)
}

func decodeJSONCursor(raw string, fallback jsonTextCursor, expectedScope string) (jsonTextCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		fallback.Scope = expectedScope
		return fallback, nil
	}
	var cursor jsonTextCursor
	if err := decodeSignedCursor(raw, &cursor); err != nil || cursor.Scope != expectedScope || cursor.Offset < 0 || cursor.Line < 1 {
		return jsonTextCursor{}, apiError{Status: http.StatusBadRequest, Code: "invalid_cursor", Message: "invalid or expired JSON continuation cursor"}
	}
	if len(cursor.Stack) > jsonMaxNestingDepth {
		return jsonTextCursor{}, apiError{Status: http.StatusBadRequest, Code: "invalid_cursor", Message: "JSON continuation cursor exceeds the nesting limit"}
	}
	for index := range cursor.Stack {
		if cursor.Stack[index] != '{' && cursor.Stack[index] != '[' {
			return jsonTextCursor{}, apiError{Status: http.StatusBadRequest, Code: "invalid_cursor", Message: "invalid JSON continuation cursor"}
		}
	}
	if cursor.Pending != 0 && cursor.Pending != 'o' && cursor.Pending != 'c' {
		return jsonTextCursor{}, apiError{Status: http.StatusBadRequest, Code: "invalid_cursor", Message: "invalid JSON continuation cursor"}
	}
	return cursor, nil
}

func consumeRawJSONState(state *jsonTextCursor, value rune) {
	if state.InString {
		if state.Escaped {
			state.Escaped = false
		} else if value == '\\' {
			state.Escaped = true
		} else if value == '"' {
			state.InString = false
		}
		return
	}
	if value == '"' {
		state.InString = true
		state.Escaped = false
	}
}

func (a *application) handleJSONRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	instance, key, etag, err := a.jsonObjectFromRequest(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	scope := signedCursorScope("json-raw", instance.cfg.ID, key)
	cursor, err := decodeJSONCursor(r.URL.Query().Get("cursor"), jsonTextCursor{Line: 1}, scope)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	body, err := openJSONObjectStream(r.Context(), instance, key, cursor.Offset, etag, cursor.Version)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer body.Close()

	reader := newJSONStreamReader(body, cursor.Offset)
	var output bytes.Buffer
	output.Grow(jsonTextPageBytes)
	startLine := cursor.Line
	line := cursor.Line
	continued := cursor.Continued
	startInString := cursor.InString
	startEscaped := cursor.Escaped
	done := false
	for output.Len() < jsonTextPageBytes && line-startLine < jsonTextPageLines {
		value, size, readErr := reader.readRune()
		if readErr != nil {
			if readErr == io.EOF {
				done = true
				break
			}
			writeAPIError(w, fmt.Errorf("read JSON source: %w", readErr))
			return
		}
		if value == utf8.RuneError && size == 1 {
			output.WriteRune(utf8.RuneError)
		} else {
			output.WriteRune(value)
		}
		if value == '\n' {
			line++
		}
		consumeRawJSONState(&cursor, value)
	}
	if output.Len() == 0 && !done {
		done = true
	}
	if !done {
		if _, peekErr := reader.peekByte(); peekErr == io.EOF {
			done = true
		} else if peekErr != nil {
			writeAPIError(w, fmt.Errorf("read JSON source: %w", peekErr))
			return
		}
	}
	next := ""
	if !done {
		text := output.String()
		next, err = encodeJSONCursor(jsonTextCursor{
			Offset:    reader.offset,
			Line:      line,
			Continued: !strings.HasSuffix(text, "\n"),
			InString:  cursor.InString,
			Escaped:   cursor.Escaped,
			Scope:     scope,
			Version:   body.Version(),
		})
		if err != nil {
			writeAPIError(w, fmt.Errorf("create JSON continuation cursor: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, jsonTextPageResponse{
		Text:          output.String(),
		NextCursor:    next,
		Done:          done,
		LineStart:     startLine,
		LineEnd:       line,
		Continued:     continued,
		StartInString: startInString,
		StartEscaped:  startEscaped,
	})
}

func writeJSONIndent(output *bytes.Buffer, depth int) {
	if depth <= 0 {
		return
	}
	output.WriteString(strings.Repeat("  ", depth))
}

func matchingJSONClose(open byte) byte {
	if open == '{' {
		return '}'
	}
	if open == '[' {
		return ']'
	}
	return 0
}

func appendJSONStack(stack string, value byte) (string, error) {
	if len(stack) >= jsonMaxNestingDepth {
		return "", apiError{Status: http.StatusUnprocessableEntity, Code: "json_too_deep", Message: "JSON nesting exceeds the supported limit"}
	}
	return stack + string(value), nil
}

func popJSONStack(stack string, close byte) (string, error) {
	if len(stack) == 0 || matchingJSONClose(stack[len(stack)-1]) != close {
		return "", apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: "JSON contains mismatched container delimiters"}
	}
	return stack[:len(stack)-1], nil
}

func writeBeautifyNewline(output *bytes.Buffer, line *int64, depth int) {
	output.WriteByte('\n')
	(*line)++
	writeJSONIndent(output, depth)
}

type jsonPageSummaryCounter struct {
	pageBytes     int64
	pageStartLine int64
	line          int64
	pages         int64
}

func newJSONPageSummaryCounter() *jsonPageSummaryCounter {
	return &jsonPageSummaryCounter{pageStartLine: 1, line: 1}
}

func (c *jsonPageSummaryCounter) writeRune(value rune) {
	size := utf8.RuneLen(value)
	if size < 0 {
		size = utf8.RuneLen(utf8.RuneError)
	}
	c.pageBytes += int64(size)
	if value == '\n' {
		c.line++
	}
}

func (c *jsonPageSummaryCounter) writeString(value string) {
	c.pageBytes += int64(len(value))
	for _, current := range value {
		if current == '\n' {
			c.line++
		}
	}
}

func (c *jsonPageSummaryCounter) writeIndent(depth int) {
	if depth > 0 {
		c.pageBytes += int64(depth * 2)
	}
}

func (c *jsonPageSummaryCounter) writeNewline(depth int) {
	c.pageBytes++
	c.line++
	c.writeIndent(depth)
}

func (c *jsonPageSummaryCounter) finishInputRune() {
	if c.pageBytes < jsonTextPageBytes && c.line-c.pageStartLine < jsonTextPageLines {
		return
	}
	c.pages++
	c.pageBytes = 0
	c.pageStartLine = c.line
}

func (c *jsonPageSummaryCounter) finishDocument() {
	if c.pageBytes > 0 || c.pages == 0 {
		c.pages++
	}
}

func consumeBeautifySummaryRune(state *jsonTextCursor, counter *jsonPageSummaryCounter, value rune) error {
processRune:
	if state.InString {
		counter.writeRune(value)
		if state.Escaped {
			state.Escaped = false
		} else if value == '\\' {
			state.Escaped = true
		} else if value == '"' {
			state.InString = false
		}
		return nil
	}

	if state.InScalar {
		if value == ' ' || value == '\t' || value == '\r' || value == '\n' || value == ',' || value == ']' || value == '}' {
			state.InScalar = false
			goto processRune
		}
		counter.writeRune(value)
		return nil
	}

	if value == ' ' || value == '\t' || value == '\r' || value == '\n' {
		return nil
	}

	if state.Pending != 0 {
		if state.Pending == 'o' && len(state.Stack) > 0 && matchingJSONClose(state.Stack[len(state.Stack)-1]) == byte(value) {
			state.Pending = 0
			stack, err := popJSONStack(state.Stack, byte(value))
			if err != nil {
				return err
			}
			state.Stack = stack
			counter.writeRune(value)
			return nil
		}
		counter.writeNewline(len(state.Stack))
		state.Pending = 0
	}

	switch value {
	case '"':
		state.InString = true
		state.Escaped = false
		counter.writeRune(value)
	case '{', '[':
		counter.writeRune(value)
		stack, err := appendJSONStack(state.Stack, byte(value))
		if err != nil {
			return err
		}
		state.Stack = stack
		state.Pending = 'o'
	case ',':
		counter.writeRune(value)
		state.Pending = 'c'
	case ':':
		counter.writeString(": ")
	case '}', ']':
		stack, err := popJSONStack(state.Stack, byte(value))
		if err != nil {
			return err
		}
		state.Stack = stack
		counter.writeNewline(len(state.Stack))
		counter.writeRune(value)
	default:
		state.InScalar = true
		counter.writeRune(value)
	}
	return nil
}

func (a *application) handleJSONSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	instance, key, etag, err := a.jsonObjectFromRequest(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	body, err := openJSONObjectStream(r.Context(), instance, key, 0, etag, objectVersion{})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer body.Close()

	reader := bufio.NewReaderSize(body, 64*1024)
	rawCounter := newJSONPageSummaryCounter()
	beautifyCounter := newJSONPageSummaryCounter()
	beautifyState := jsonTextCursor{Line: 1}

	var rawPageOutput bytes.Buffer
	rawPageOutput.Grow(jsonTextPageBytes)
	rawPageState := jsonTextCursor{Line: 1}
	rawPageBoundary := false
	rawPageHasMore := false
	var rawPageOffset int64
	var rawPageCursorState jsonTextCursor

	for {
		value, size, readErr := reader.ReadRune()
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			writeAPIError(w, fmt.Errorf("read JSON source: %w", readErr))
			return
		}
		rawPageOffset += int64(size)
		if value == utf8.RuneError && size == 1 {
			value = utf8.RuneError
		}

		if !rawPageBoundary {
			rawPageOutput.WriteRune(value)
			if value == '\n' {
				rawPageState.Line++
			}
			consumeRawJSONState(&rawPageState, value)
			if rawPageOutput.Len() >= jsonTextPageBytes || rawPageState.Line-1 >= jsonTextPageLines {
				rawPageBoundary = true
				rawPageCursorState = jsonTextCursor{
					Offset:    rawPageOffset,
					Line:      rawPageState.Line,
					Continued: !strings.HasSuffix(rawPageOutput.String(), "\n"),
					InString:  rawPageState.InString,
					Escaped:   rawPageState.Escaped,
				}
			}
		} else {
			rawPageHasMore = true
		}

		rawCounter.writeRune(value)
		rawCounter.finishInputRune()
		if err := consumeBeautifySummaryRune(&beautifyState, beautifyCounter, value); err != nil {
			writeAPIError(w, err)
			return
		}
		beautifyCounter.finishInputRune()
	}
	if beautifyState.InString || beautifyState.Escaped || len(beautifyState.Stack) != 0 {
		writeAPIError(w, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: "JSON ended before all strings or containers were closed"})
		return
	}
	rawCounter.finishDocument()
	beautifyCounter.finishDocument()

	rawPage := &jsonTextPageResponse{
		Text:      rawPageOutput.String(),
		Done:      !rawPageHasMore,
		LineStart: 1,
		LineEnd:   rawPageState.Line,
	}
	if rawPageHasMore {
		rawPageCursorState.Scope = signedCursorScope("json-raw", instance.cfg.ID, key)
		rawPageCursorState.Version = body.Version()
		rawPage.NextCursor, err = encodeJSONCursor(rawPageCursorState)
		if err != nil {
			writeAPIError(w, fmt.Errorf("create JSON summary cursor: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, jsonSummaryResponse{
		RawLines:      rawCounter.line,
		RawPages:      rawCounter.pages,
		BeautifyLines: beautifyCounter.line,
		BeautifyPages: beautifyCounter.pages,
		RawPage:       rawPage,
	})
}

func (a *application) handleJSONBeautify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	instance, key, etag, err := a.jsonObjectFromRequest(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	scope := signedCursorScope("json-beautify", instance.cfg.ID, key)
	cursor, err := decodeJSONCursor(r.URL.Query().Get("cursor"), jsonTextCursor{Line: 1}, scope)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	body, err := openJSONObjectStream(r.Context(), instance, key, cursor.Offset, etag, cursor.Version)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer body.Close()

	reader := newJSONStreamReader(body, cursor.Offset)
	state := cursor
	startLine := state.Line
	var output bytes.Buffer
	output.Grow(jsonTextPageBytes)
	done := false

	for output.Len() < jsonTextPageBytes && state.Line-startLine < jsonTextPageLines {
		value, size, readErr := reader.readRune()
		if readErr != nil {
			if readErr == io.EOF {
				done = true
				break
			}
			writeAPIError(w, fmt.Errorf("read JSON source: %w", readErr))
			return
		}
		if value == utf8.RuneError && size == 1 {
			value = utf8.RuneError
		}

	processRune:
		if state.InString {
			output.WriteRune(value)
			if state.Escaped {
				state.Escaped = false
			} else if value == '\\' {
				state.Escaped = true
			} else if value == '"' {
				state.InString = false
			}
			continue
		}

		if state.InScalar {
			if value == ' ' || value == '\t' || value == '\r' || value == '\n' || value == ',' || value == ']' || value == '}' {
				state.InScalar = false
				goto processRune
			}
			output.WriteRune(value)
			continue
		}

		if value == ' ' || value == '\t' || value == '\r' || value == '\n' {
			continue
		}

		if state.Pending != 0 {
			if state.Pending == 'o' && len(state.Stack) > 0 && matchingJSONClose(state.Stack[len(state.Stack)-1]) == byte(value) {
				state.Pending = 0
				state.Stack, err = popJSONStack(state.Stack, byte(value))
				if err != nil {
					writeAPIError(w, err)
					return
				}
				output.WriteRune(value)
				continue
			}
			writeBeautifyNewline(&output, &state.Line, len(state.Stack))
			state.Pending = 0
		}

		switch value {
		case '"':
			state.InString = true
			state.Escaped = false
			output.WriteRune(value)
		case '{', '[':
			output.WriteRune(value)
			state.Stack, err = appendJSONStack(state.Stack, byte(value))
			if err != nil {
				writeAPIError(w, err)
				return
			}
			state.Pending = 'o'
		case ',':
			output.WriteRune(value)
			state.Pending = 'c'
		case ':':
			output.WriteString(": ")
		case '}', ']':
			state.Stack, err = popJSONStack(state.Stack, byte(value))
			if err != nil {
				writeAPIError(w, err)
				return
			}
			writeBeautifyNewline(&output, &state.Line, len(state.Stack))
			output.WriteRune(value)
		default:
			state.InScalar = true
			output.WriteRune(value)
		}
	}

	if !done {
		if _, peekErr := reader.peekByte(); peekErr == io.EOF {
			done = true
		} else if peekErr != nil {
			writeAPIError(w, fmt.Errorf("read JSON source: %w", peekErr))
			return
		}
	}
	if done {
		if state.InString || state.Escaped || len(state.Stack) != 0 {
			writeAPIError(w, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: "JSON ended before all strings or containers were closed"})
			return
		}
		if state.Pending != 0 {
			state.Pending = 0
		}
	}
	state.Offset = reader.offset
	next := ""
	if !done {
		state.Scope = scope
		state.Version = body.Version()
		next, err = encodeJSONCursor(state)
		if err != nil {
			writeAPIError(w, fmt.Errorf("create JSON continuation cursor: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, jsonTextPageResponse{
		Text:          output.String(),
		NextCursor:    next,
		Done:          done,
		LineStart:     startLine,
		LineEnd:       state.Line,
		Continued:     cursor.InString || cursor.InScalar,
		StartInString: cursor.InString,
		StartEscaped:  cursor.Escaped,
	})
}

func jsonStringPreview(raw []byte, complete bool) string {
	if complete {
		quoted := append([]byte{'"'}, raw...)
		quoted = append(quoted, '"')
		var value string
		if err := json.Unmarshal(quoted, &value); err == nil {
			return value
		}
	}
	value := strings.ReplaceAll(string(raw), `\n`, "↵")
	value = strings.ReplaceAll(value, `\r`, "")
	value = strings.ReplaceAll(value, `\t`, "⇥")
	value = strings.ReplaceAll(value, `\"`, `"`)
	value = strings.ReplaceAll(value, `\\`, `\`)
	return strings.ToValidUTF8(value, "�")
}

func readJSONString(reader *jsonStreamReader, captureLimit int) (start, end int64, preview string, truncated bool, err error) {
	start = reader.offset
	opening, err := reader.readByte()
	if err != nil {
		return 0, 0, "", false, err
	}
	if opening != '"' {
		return 0, 0, "", false, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: fmt.Sprintf("expected JSON string at byte %d", start)}
	}
	captured := make([]byte, 0, minInt(captureLimit, 256))
	escaped := false
	for {
		value, readErr := reader.readByte()
		if readErr != nil {
			if readErr == io.EOF {
				return 0, 0, "", false, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: "JSON ended inside a string"}
			}
			return 0, 0, "", false, readErr
		}
		if escaped {
			if len(captured)+2 <= captureLimit {
				captured = append(captured, '\\', value)
			} else {
				truncated = true
			}
			escaped = false
			continue
		}
		if value == '\\' {
			escaped = true
			continue
		}
		if value == '"' {
			return start, reader.offset, jsonStringPreview(captured, !truncated), truncated, nil
		}
		if len(captured) < captureLimit {
			captured = append(captured, value)
		} else {
			truncated = true
		}
	}
}

func skipJSONContainer(reader *jsonStreamReader) (int64, int64, error) {
	opening, err := reader.readByte()
	if err != nil {
		return 0, 0, err
	}
	if opening != '{' && opening != '[' {
		return 0, 0, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: "expected a JSON object or array"}
	}
	stack := []byte{opening}
	inString := false
	escaped := false
	expectEntry := true
	var directChildren int64
	for len(stack) > 0 {
		value, readErr := reader.readByte()
		if readErr != nil {
			if readErr == io.EOF {
				return 0, 0, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: "JSON ended before a container was closed"}
			}
			return 0, 0, readErr
		}
		if inString {
			if escaped {
				escaped = false
			} else if value == '\\' {
				escaped = true
			} else if value == '"' {
				inString = false
			}
			continue
		}
		if len(stack) == 1 {
			if expectEntry {
				switch value {
				case ' ', '\t', '\r', '\n':
					continue
				case matchingJSONClose(opening):
					stack = stack[:0]
					continue
				default:
					directChildren++
					expectEntry = false
				}
			} else if value == ',' {
				expectEntry = true
				continue
			}
		}
		switch value {
		case '"':
			inString = true
		case '{', '[':
			if len(stack) >= jsonMaxNestingDepth {
				return 0, 0, apiError{Status: http.StatusUnprocessableEntity, Code: "json_too_deep", Message: "JSON nesting exceeds the supported limit"}
			}
			stack = append(stack, value)
		case '}', ']':
			if matchingJSONClose(stack[len(stack)-1]) != value {
				return 0, 0, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: fmt.Sprintf("mismatched JSON delimiter at byte %d", reader.offset-1)}
			}
			stack = stack[:len(stack)-1]
		}
	}
	return reader.offset, directChildren, nil
}

func isJSONScalarDelimiter(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n', ',', ']', '}':
		return true
	default:
		return false
	}
}

func inspectJSONValue(reader *jsonStreamReader) (jsonTreeNodeJSON, error) {
	if err := reader.skipWhitespace(); err != nil {
		return jsonTreeNodeJSON{}, err
	}
	start := reader.offset
	value, err := reader.peekByte()
	if err != nil {
		return jsonTreeNodeJSON{}, err
	}
	switch value {
	case '{', '[':
		kind := "object"
		preview := "{…}"
		if value == '[' {
			kind = "array"
			preview = "[…]"
		}
		end, count, err := skipJSONContainer(reader)
		if err != nil {
			return jsonTreeNodeJSON{}, err
		}
		return jsonTreeNodeJSON{Type: kind, Preview: preview, Container: true, Start: start, End: end, Count: count, CountKnown: true}, nil
	case '"':
		_, end, preview, truncated, err := readJSONString(reader, jsonTreePreviewBytes)
		if err != nil {
			return jsonTreeNodeJSON{}, err
		}
		if truncated {
			preview += "…"
		}
		return jsonTreeNodeJSON{Type: "string", Preview: `"` + preview + `"`, Start: start, End: end}, nil
	default:
		captured := make([]byte, 0, 64)
		truncated := false
		for {
			current, peekErr := reader.peekByte()
			if peekErr != nil {
				if peekErr == io.EOF {
					break
				}
				return jsonTreeNodeJSON{}, peekErr
			}
			if isJSONScalarDelimiter(current) {
				break
			}
			_, _ = reader.readByte()
			if len(captured) < jsonTreePreviewBytes {
				captured = append(captured, current)
			} else {
				truncated = true
			}
		}
		raw := strings.TrimSpace(string(captured))
		kind := "number"
		if raw == "true" || raw == "false" {
			kind = "boolean"
		} else if raw == "null" {
			kind = "null"
		} else if _, parseErr := strconv.ParseFloat(raw, 64); parseErr != nil {
			kind = "unknown"
		}
		if truncated {
			raw += "…"
		}
		return jsonTreeNodeJSON{Type: kind, Preview: raw, Start: start, End: reader.offset}, nil
	}
}

func readJSONTreePage(reader *jsonStreamReader, node jsonTreeNodeJSON, cursorProvided bool, nextIndex int64, limit int) ([]jsonTreeNodeJSON, int64, int64, bool, error) {
	if node.Type != "object" && node.Type != "array" {
		return nil, 0, nextIndex, true, nil
	}
	if !cursorProvided {
		if err := reader.skipWhitespace(); err != nil {
			return nil, 0, nextIndex, false, err
		}
		opening, err := reader.readByte()
		if err != nil {
			return nil, 0, nextIndex, false, err
		}
		expected := byte('{')
		if node.Type == "array" {
			expected = '['
		}
		if opening != expected {
			return nil, 0, nextIndex, false, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: fmt.Sprintf("JSON %s no longer starts at byte %d", node.Type, node.Start)}
		}
	}

	closing := byte('}')
	if node.Type == "array" {
		closing = ']'
	}
	children := make([]jsonTreeNodeJSON, 0, limit)
	for len(children) < limit {
		if err := reader.skipWhitespace(); err != nil {
			if err == io.EOF {
				return nil, 0, nextIndex, false, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: "JSON ended while reading container children"}
			}
			return nil, 0, nextIndex, false, err
		}
		current, err := reader.peekByte()
		if err != nil {
			return nil, 0, nextIndex, false, err
		}
		if current == closing {
			_, _ = reader.readByte()
			return children, 0, nextIndex, true, nil
		}

		label := strconv.FormatInt(nextIndex, 10)
		if node.Type == "object" {
			_, _, keyPreview, truncated, err := readJSONString(reader, jsonTreeKeyPreviewBytes)
			if err != nil {
				return nil, 0, nextIndex, false, err
			}
			label = keyPreview
			if truncated {
				label += "…"
			}
			if err := reader.skipWhitespace(); err != nil {
				return nil, 0, nextIndex, false, err
			}
			colon, err := reader.readByte()
			if err != nil || colon != ':' {
				return nil, 0, nextIndex, false, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: fmt.Sprintf("expected ':' after JSON property at byte %d", reader.offset)}
			}
		}
		child, err := inspectJSONValue(reader)
		if err != nil {
			return nil, 0, nextIndex, false, err
		}
		child.Label = label
		children = append(children, child)
		nextIndex++

		if err := reader.skipWhitespace(); err != nil {
			return nil, 0, nextIndex, false, err
		}
		separator, err := reader.readByte()
		if err != nil {
			return nil, 0, nextIndex, false, err
		}
		switch separator {
		case ',':
			if len(children) >= limit {
				return children, reader.offset, nextIndex, false, nil
			}
		case closing:
			return children, 0, nextIndex, true, nil
		default:
			return nil, 0, nextIndex, false, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: fmt.Sprintf("expected ',' or closing delimiter at byte %d", reader.offset-1)}
		}
	}
	return children, reader.offset, nextIndex, false, nil
}

func (a *application) handleJSONTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	instance, key, etag, err := a.jsonObjectFromRequest(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	start, err := parseNonNegativeInt64(r.URL.Query().Get("start"), 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	scope := signedCursorScope("json-tree", instance.cfg.ID, key)
	cursorState, cursorProvided, err := decodeJSONTreeCursor(r.URL.Query().Get("cursor"), scope)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	nextIndex, err := parseNonNegativeInt64(r.URL.Query().Get("index"), 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	limit, err := parsePositiveInt(r.URL.Query().Get("limit"), jsonTreeDefaultPageSize, jsonTreeMaxPageSize)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	nodeType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	if nodeType != "" && nodeType != "object" && nodeType != "array" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_node_type", Message: "JSON tree node type must be object or array"})
		return
	}

	streamOffset := start
	version := objectVersion{ETag: etag}
	if cursorProvided {
		if start != 0 && start != cursorState.Start {
			writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_cursor_scope", Message: "the JSON tree cursor does not belong to this node"})
			return
		}
		if nodeType != "" && nodeType != cursorState.Type {
			writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_cursor_scope", Message: "the JSON tree cursor does not belong to this node type"})
			return
		}
		streamOffset = cursorState.Offset
		start = cursorState.Start
		nextIndex = cursorState.Index
		nodeType = cursorState.Type
		version = cursorState.Version
	}
	body, err := openJSONObjectStream(r.Context(), instance, key, streamOffset, etag, version)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	defer body.Close()
	reader := newJSONStreamReader(body, streamOffset)

	var node jsonTreeNodeJSON
	if nodeType == "" {
		if err := reader.skipWhitespace(); err != nil {
			writeAPIError(w, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_json", Message: "JSON document is empty"})
			return
		}
		rootStart := reader.offset
		opening, err := reader.peekByte()
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if opening == '{' || opening == '[' {
			node = jsonTreeNodeJSON{Type: "object", Preview: "{…}", Container: true, Start: rootStart}
			if opening == '[' {
				node.Type = "array"
				node.Preview = "[…]"
			}
		} else {
			node, err = inspectJSONValue(reader)
			if err != nil {
				writeAPIError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, jsonTreeResponse{Node: node, Done: true})
			return
		}
	} else {
		node = jsonTreeNodeJSON{Type: nodeType, Container: true, Start: start}
		if nodeType == "object" {
			node.Preview = "{…}"
		} else {
			node.Preview = "[…]"
		}
	}

	children, nextOffset, nextIndex, done, err := readJSONTreePage(reader, node, cursorProvided, nextIndex, limit)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if done {
		node.Count = nextIndex
		node.CountKnown = true
	}
	nextCursor := ""
	if !done && nextOffset > 0 {
		nextCursor, err = encodeSignedCursor(jsonTreeCursor{
			Offset:  nextOffset,
			Index:   nextIndex,
			Start:   node.Start,
			Type:    node.Type,
			Scope:   scope,
			Version: body.Version(),
		})
		if err != nil {
			writeAPIError(w, fmt.Errorf("create JSON tree continuation cursor: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, jsonTreeResponse{
		Node:      node,
		Children:  children,
		Cursor:    nextCursor,
		NextIndex: nextIndex,
		Done:      done,
	})
}
