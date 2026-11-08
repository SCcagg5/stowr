package main

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxWordObjectBytes   = int64(8 << 30)
	maxWordXMLBytes      = int64(64 << 20)
	maxWordArchiveFiles  = 20_000
	maxWordBlocks        = 20_000
	maxWordTextBytes     = 8 << 20
	maxWordTableCells    = 200_000
	maxWordCellTextBytes = 1 << 20
)

var errWordXMLLimit = errors.New("word document XML exceeds the preview limit")

type wordPreviewBlock struct {
	Type    string     `json:"type"`
	Text    string     `json:"text,omitempty"`
	Level   int        `json:"level,omitempty"`
	Ordered bool       `json:"ordered,omitempty"`
	Rows    [][]string `json:"rows,omitempty"`
}

type wordPreviewResponse struct {
	Instance        string             `json:"instance"`
	Key             string             `json:"key"`
	Blocks          []wordPreviewBlock `json:"blocks"`
	Truncated       bool               `json:"truncated,omitempty"`
	MacrosIgnored   bool               `json:"macrosIgnored,omitempty"`
	StorageBytes    int64              `json:"storageBytes"`
	StorageRequests int                `json:"storageRequests"`
}

type hardLimitReader struct {
	reader io.Reader
	limit  int64
	read   int64
}

func (r *hardLimitReader) Read(destination []byte) (int, error) {
	if r == nil || r.reader == nil {
		return 0, io.EOF
	}
	if r.read >= r.limit {
		return 0, errWordXMLLimit
	}
	remaining := r.limit - r.read
	if int64(len(destination)) > remaining {
		destination = destination[:remaining]
	}
	count, err := r.reader.Read(destination)
	r.read += int64(count)
	return count, err
}

type wordPreviewLimits struct {
	blocks    int
	textBytes int
	cells     int
	truncated bool
}

func (l *wordPreviewLimits) addText(value string) string {
	if l == nil || value == "" {
		return value
	}
	remaining := maxWordTextBytes - l.textBytes
	if remaining <= 0 {
		l.truncated = true
		return ""
	}
	if len(value) > remaining {
		value = truncateUTF8Bytes(value, remaining)
		l.truncated = true
	}
	l.textBytes += len(value)
	return value
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 || value == "" {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func (a *application) handleWordPreview(w http.ResponseWriter, r *http.Request) {
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
	key := strings.TrimLeft(r.URL.Query().Get("key"), "/")
	if key == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "missing_key", Message: "key is required"})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}
	knownSize, err := parseOptionalObjectSize(r.URL.Query().Get("size"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	response, err := readWordPreview(r.Context(), instance, key, knownSize, r.URL.Query().Get("etag"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func parseOptionalObjectSize(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, apiError{Status: http.StatusBadRequest, Code: "invalid_size", Message: "size must be a non-negative integer"}
	}
	return value, nil
}

func readWordPreview(ctx context.Context, instance *storageInstance, key string, knownSize int64, expectedETag string) (wordPreviewResponse, error) {
	response := wordPreviewResponse{Instance: instance.cfg.ID, Key: key}
	source, err := openObjectRangeSource(ctx, instance, key)
	if err != nil {
		return response, err
	}
	source.SetKnownSize(knownSize)
	source.SetExpectedETag(expectedETag)
	if source.Size() <= 0 {
		if _, err := source.ReadSuffix(64 << 10); err != nil && !errors.Is(err, io.EOF) {
			return response, err
		}
	}
	if source.Size() <= 0 {
		return response, apiError{Status: http.StatusUnprocessableEntity, Code: "unknown_document_size", Message: "DOCX preview requires a known non-zero object size"}
	}
	if source.Size() > maxWordObjectBytes {
		return response, apiError{Status: http.StatusRequestEntityTooLarge, Code: "document_too_large", Message: fmt.Sprintf("DOCX previews are limited to objects smaller than %d GiB", maxWordObjectBytes>>30)}
	}

	reader, err := zip.NewReader(&spreadsheetObjectReaderAt{source: source}, source.Size())
	if err != nil {
		return response, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_docx", Message: "the object is not a valid DOCX package"}
	}
	if len(reader.File) > maxWordArchiveFiles {
		return response, apiError{Status: http.StatusRequestEntityTooLarge, Code: "document_archive_too_large", Message: "the DOCX package contains too many archive members"}
	}
	var document *zip.File
	for _, file := range reader.File {
		name := path.Clean(strings.TrimLeft(file.Name, "/"))
		if name == "word/document.xml" {
			document = file
			break
		}
	}
	if document == nil {
		return response, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_docx", Message: "the DOCX package is missing word/document.xml"}
	}
	if document.UncompressedSize64 > uint64(maxWordXMLBytes) {
		return response, apiError{Status: http.StatusRequestEntityTooLarge, Code: "document_xml_too_large", Message: "the DOCX document body exceeds the preview limit"}
	}
	stream, err := document.Open()
	if err != nil {
		return response, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_docx", Message: "unable to open the DOCX document body"}
	}
	defer stream.Close()

	limits := &wordPreviewLimits{}
	blocks, parseErr := parseWordDocumentXML(&hardLimitReader{reader: stream, limit: maxWordXMLBytes + 1}, limits)
	if parseErr != nil {
		if errors.Is(parseErr, errWordXMLLimit) {
			return response, apiError{Status: http.StatusRequestEntityTooLarge, Code: "document_xml_too_large", Message: "the DOCX document body exceeds the preview limit"}
		}
		return response, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_docx_xml", Message: "the DOCX document body could not be parsed"}
	}
	response.Blocks = blocks
	response.Truncated = limits.truncated
	response.MacrosIgnored = strings.EqualFold(path.Ext(key), ".docm")
	response.StorageBytes = source.BytesRead()
	response.StorageRequests = source.RequestCount()
	return response, nil
}

func parseWordDocumentXML(reader io.Reader, limits *wordPreviewLimits) ([]wordPreviewBlock, error) {
	decoder := xml.NewDecoder(reader)
	blocks := make([]wordPreviewBlock, 0, 64)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if limits.blocks >= maxWordBlocks || limits.textBytes >= maxWordTextBytes || limits.cells >= maxWordTableCells {
			limits.truncated = true
			break
		}
		switch start.Name.Local {
		case "p":
			block, err := parseWordParagraph(decoder, start, limits)
			if err != nil {
				return nil, err
			}
			if block.Text != "" {
				blocks = append(blocks, block)
				limits.blocks++
			}
		case "tbl":
			block, err := parseWordTable(decoder, start, limits)
			if err != nil {
				return nil, err
			}
			if len(block.Rows) > 0 {
				blocks = append(blocks, block)
				limits.blocks++
			}
		}
	}
	return blocks, nil
}

func parseWordParagraph(decoder *xml.Decoder, _ xml.StartElement, limits *wordPreviewLimits) (wordPreviewBlock, error) {
	block := wordPreviewBlock{Type: "paragraph"}
	var text strings.Builder
	depth := 1
	inText := false
	style := ""
	outlineLevel := 0
	listLevel := 0
	list := false
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return block, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			switch value.Name.Local {
			case "t", "delText", "instrText":
				inText = true
			case "tab":
				text.WriteByte('\t')
			case "br", "cr":
				text.WriteByte('\n')
			case "pStyle":
				style = wordXMLAttribute(value, "val")
			case "outlineLvl":
				if parsed, err := strconv.Atoi(wordXMLAttribute(value, "val")); err == nil {
					outlineLevel = parsed + 1
				}
			case "numPr":
				list = true
			case "ilvl":
				if parsed, err := strconv.Atoi(wordXMLAttribute(value, "val")); err == nil {
					listLevel = parsed + 1
				}
			}
		case xml.CharData:
			if inText {
				text.Write([]byte(value))
			}
		case xml.EndElement:
			if value.Name.Local == "t" || value.Name.Local == "delText" || value.Name.Local == "instrText" {
				inText = false
			}
			depth--
		}
	}
	block.Text = limits.addText(normalizeWordText(text.String()))
	if block.Text == "" {
		return block, nil
	}
	if level := wordHeadingLevel(style, outlineLevel); level > 0 {
		block.Type = "heading"
		block.Level = level
	} else if list {
		block.Type = "list-item"
		block.Level = maxInt(1, listLevel)
		block.Ordered = false
	}
	return block, nil
}

func parseWordTable(decoder *xml.Decoder, _ xml.StartElement, limits *wordPreviewLimits) (wordPreviewBlock, error) {
	block := wordPreviewBlock{Type: "table"}
	depth := 1
	for depth > 0 {
		if limits.cells >= maxWordTableCells || limits.textBytes >= maxWordTextBytes {
			limits.truncated = true
			if err := skipXMLContainer(decoder, depth); err != nil {
				return block, err
			}
			break
		}
		token, err := decoder.Token()
		if err != nil {
			return block, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if value.Name.Local == "tr" {
				row, err := parseWordTableRow(decoder, value, limits)
				if err != nil {
					return block, err
				}
				depth--
				if len(row) > 0 {
					block.Rows = append(block.Rows, row)
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	return block, nil
}

func parseWordTableRow(decoder *xml.Decoder, _ xml.StartElement, limits *wordPreviewLimits) ([]string, error) {
	row := make([]string, 0, 8)
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			if value.Name.Local == "tc" {
				cell, err := parseWordTableCell(decoder, value, limits)
				if err != nil {
					return nil, err
				}
				depth--
				row = append(row, cell)
				limits.cells++
				if limits.cells >= maxWordTableCells {
					limits.truncated = true
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	return row, nil
}

func parseWordTableCell(decoder *xml.Decoder, _ xml.StartElement, limits *wordPreviewLimits) (string, error) {
	var text strings.Builder
	depth := 1
	inText := false
	paragraphHasText := false
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch value := token.(type) {
		case xml.StartElement:
			depth++
			switch value.Name.Local {
			case "t", "delText":
				inText = true
			case "tab":
				text.WriteByte('\t')
			case "br", "cr":
				text.WriteByte('\n')
			}
		case xml.CharData:
			if inText && text.Len() < maxWordCellTextBytes {
				text.Write([]byte(value))
				paragraphHasText = true
			}
		case xml.EndElement:
			if value.Name.Local == "t" || value.Name.Local == "delText" {
				inText = false
			}
			if value.Name.Local == "p" && paragraphHasText && depth > 1 {
				text.WriteByte('\n')
				paragraphHasText = false
			}
			depth--
		}
		if text.Len() >= maxWordCellTextBytes {
			limits.truncated = true
		}
	}
	return limits.addText(normalizeWordText(text.String())), nil
}

func skipXMLContainer(decoder *xml.Decoder, depth int) error {
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

func normalizeWordText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		lines[index] = strings.TrimFunc(line, unicode.IsSpace)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func wordXMLAttribute(start xml.StartElement, local string) string {
	for _, attribute := range start.Attr {
		if attribute.Name.Local == local {
			return strings.TrimSpace(attribute.Value)
		}
	}
	return ""
}

func wordHeadingLevel(style string, outlineLevel int) int {
	if outlineLevel > 0 {
		return minInt(6, outlineLevel)
	}
	lower := strings.ToLower(strings.TrimSpace(style))
	if !strings.HasPrefix(lower, "heading") && !strings.HasPrefix(lower, "titre") {
		return 0
	}
	for index := len(lower) - 1; index >= 0; index-- {
		if lower[index] < '0' || lower[index] > '9' {
			if index == len(lower)-1 {
				return 1
			}
			level, _ := strconv.Atoi(lower[index+1:])
			return maxInt(1, minInt(6, level))
		}
	}
	return 1
}
