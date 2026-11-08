package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	maxSpreadsheetObjectBytes = int64(512 << 20)
	maxSpreadsheetRows        = 2_000_000
	maxSpreadsheetColumns     = 200
	maxSpreadsheetSortRows    = 150_000
	maxSharedStrings          = 2_000_000
	maxSharedStringBytes      = 128 << 20
	maxSpreadsheetSheetBytes  = 256 << 20
	maxSpreadsheetCellBytes   = 32 << 10
	// Shared-string cells alias one table entry, so the response and the
	// search/filter work grow with references, not with the table size.
	maxSpreadsheetReferencedBytes = int64(1 << 30)
	maxSpreadsheetPageBytes       = 32 << 20
	spreadsheetContextCheckEvery  = 1024
)

type spreadsheetSheetInfo struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Hidden string `json:"hidden,omitempty"`
	Target string `json:"-"`
}

type spreadsheetRowJSON struct {
	Number int      `json:"number"`
	Cells  []string `json:"cells"`
}

type spreadsheetResponseJSON struct {
	Instance         string                 `json:"instance"`
	Key              string                 `json:"key"`
	Sheets           []spreadsheetSheetInfo `json:"sheets"`
	Sheet            spreadsheetSheetInfo   `json:"sheet"`
	Headers          []string               `json:"headers"`
	Rows             []spreadsheetRowJSON   `json:"rows"`
	TotalRows        int                    `json:"totalRows"`
	SourceRows       int                    `json:"sourceRows"`
	Page             int                    `json:"page"`
	PageSize         int                    `json:"pageSize"`
	TruncatedRows    bool                   `json:"truncatedRows,omitempty"`
	TruncatedColumns bool                   `json:"truncatedColumns,omitempty"`
	TruncatedCells   bool                   `json:"truncatedCells,omitempty"`
	MacrosIgnored    bool                   `json:"macrosIgnored,omitempty"`
}

type workbookRelationshipXML struct {
	ID     string `xml:"Id,attr"`
	Target string `xml:"Target,attr"`
	Type   string `xml:"Type,attr"`
}

type workbookRelationshipsXML struct {
	Relationships []workbookRelationshipXML `xml:"Relationship"`
}

type workbookSheetXML struct {
	Name  string `xml:"name,attr"`
	State string `xml:"state,attr"`
	RelID string `xml:"id,attr"`
}

type workbookViewXML struct {
	ActiveTab int `xml:"activeTab,attr"`
}

type workbookXML struct {
	Views  []workbookViewXML  `xml:"bookViews>workbookView"`
	Sheets []workbookSheetXML `xml:"sheets>sheet"`
}

type numberFormatXML struct {
	ID   int    `xml:"numFmtId,attr"`
	Code string `xml:"formatCode,attr"`
}

type cellFormatXML struct {
	NumberFormatID int `xml:"numFmtId,attr"`
}

type workbookStylesXML struct {
	NumberFormats []numberFormatXML `xml:"numFmts>numFmt"`
	CellFormats   []cellFormatXML   `xml:"cellXfs>xf"`
}

type spreadsheetStyles struct {
	dateStyles map[int]bool
}

type spreadsheetRecord struct {
	rowNumber int
	cells     map[int]string
}

// spreadsheetReadState carries the per-request work budget of one worksheet
// parse: request cancellation, bytes expanded from shared strings and whether
// any cell was cut to the per-cell limit.
type spreadsheetReadState struct {
	ctx             context.Context
	ticks           int
	referencedBytes int64
	truncatedCells  bool
}

func (s *spreadsheetReadState) tick() error {
	s.ticks++
	if s.ticks%spreadsheetContextCheckEvery == 0 {
		return s.ctx.Err()
	}
	return nil
}

type spreadsheetQuery struct {
	page          int
	pageSize      int
	filters       map[int]string
	globalSearch  string
	sortColumn    int
	sortDirection string
}

func (a *application) handleSpreadsheet(w http.ResponseWriter, r *http.Request) {
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
	if key == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "spreadsheet key cannot be empty"})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}
	extension := strings.ToLower(strings.TrimPrefix(filepath.Ext(key), "."))
	if extension != "xlsx" && extension != "xlsm" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "unsupported_spreadsheet", Message: "server-side spreadsheet preview supports XLSX and XLSM files"})
		return
	}
	query, err := parseSpreadsheetQuery(r)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	knownSize := int64(parseBoundedInt(r.URL.Query().Get("size"), 0, 0, int(maxSpreadsheetObjectBytes)))
	reader, err := openSpreadsheetReader(r.Context(), instance, key, knownSize)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	files := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		files[path.Clean(strings.TrimLeft(file.Name, "/"))] = file
	}
	sheets, err := readWorkbookSheets(files)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if len(sheets) == 0 {
		writeAPIError(w, apiError{Status: http.StatusUnprocessableEntity, Code: "empty_workbook", Message: "the workbook does not contain any worksheets"})
		return
	}
	sheetIndex := parseBoundedInt(r.URL.Query().Get("sheet"), 0, 0, len(sheets)-1)
	if sheetIndex >= len(sheets) {
		sheetIndex = 0
	}
	sharedStrings, err := readSharedStrings(files["xl/sharedStrings.xml"])
	if err != nil {
		writeAPIError(w, err)
		return
	}
	styles, err := readSpreadsheetStyles(files["xl/styles.xml"])
	if err != nil {
		writeAPIError(w, err)
		return
	}
	response, err := readSpreadsheetSheet(r.Context(), files[sheets[sheetIndex].Target], sheets, sheetIndex, sharedStrings, styles, query)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	response.Instance = instance.cfg.ID
	response.Key = key
	response.MacrosIgnored = extension == "xlsm"
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func parseSpreadsheetQuery(r *http.Request) (spreadsheetQuery, error) {
	query := spreadsheetQuery{
		page:         parseBoundedInt(r.URL.Query().Get("page"), 0, 0, math.MaxInt32),
		pageSize:     parseBoundedInt(r.URL.Query().Get("pageSize"), 100, 1, 1000),
		filters:      make(map[int]string),
		globalSearch: strings.TrimSpace(r.URL.Query().Get("search")),
		sortColumn:   -1,
	}
	if len(query.globalSearch) > 1024 {
		return spreadsheetQuery{}, apiError{Status: http.StatusBadRequest, Code: "search_query_too_long", Message: "spreadsheet search queries are limited to 1024 characters"}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("filters")); raw != "" {
		var filters map[string]string
		if err := json.Unmarshal([]byte(raw), &filters); err != nil {
			return spreadsheetQuery{}, apiError{Status: http.StatusBadRequest, Code: "invalid_filters", Message: "filters must be a JSON object keyed by Excel column name"}
		}
		for name, value := range filters {
			column, ok := excelColumnIndex(name)
			if !ok || column >= maxSpreadsheetColumns {
				return spreadsheetQuery{}, apiError{Status: http.StatusBadRequest, Code: "invalid_filter_column", Message: fmt.Sprintf("invalid filter column %q", name)}
			}
			if strings.TrimSpace(value) != "" {
				query.filters[column] = value
			}
		}
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("sortColumn")); raw != "" {
		column, ok := excelColumnIndex(raw)
		if !ok || column >= maxSpreadsheetColumns {
			return spreadsheetQuery{}, apiError{Status: http.StatusBadRequest, Code: "invalid_sort_column", Message: "sortColumn must be a visible Excel column name"}
		}
		query.sortColumn = column
		query.sortDirection = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sortDirection")))
		if query.sortDirection != "asc" && query.sortDirection != "desc" {
			return spreadsheetQuery{}, apiError{Status: http.StatusBadRequest, Code: "invalid_sort_direction", Message: "sortDirection must be asc or desc"}
		}
	}
	return query, nil
}

const spreadsheetRangeBlockBytes = int64(1 << 20)

type spreadsheetObjectReaderAt struct {
	source *objectRangeSource
	mu     sync.Mutex
	err    error
}

var binaryExcelCompoundSignature = []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}

func (r *spreadsheetObjectReaderAt) rememberError(err error) {
	if r == nil || err == nil || errors.Is(err, io.EOF) {
		return
	}
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.mu.Unlock()
}

func (r *spreadsheetObjectReaderAt) Error() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func spreadsheetContainerError(signature []byte) error {
	if len(signature) >= len(binaryExcelCompoundSignature) && string(signature[:len(binaryExcelCompoundSignature)]) == string(binaryExcelCompoundSignature) {
		return apiError{
			Status:  http.StatusUnprocessableEntity,
			Code:    "unsupported_xls_container",
			Message: "this file uses the binary XLS format even though its name ends in .xlsx or .xlsm; download it or convert it to XLSX or CSV",
		}
	}
	return apiError{
		Status:  http.StatusUnprocessableEntity,
		Code:    "invalid_workbook_container",
		Message: "the file extension says XLSX/XLSM, but the object is not an Office ZIP package; verify the source file or download the original",
	}
}

func newSpreadsheetZipReader(source *objectRangeSource) (*zip.Reader, error) {
	if source == nil || source.Size() <= 0 {
		return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "unknown_workbook_size", Message: "spreadsheet preview requires a known non-zero object size"}
	}

	headerLength := minInt64(8, source.Size())
	signature, err := source.ReadRange(0, headerLength)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	isZIP := len(signature) >= 4 && signature[0] == 'P' && signature[1] == 'K' &&
		((signature[2] == 3 && signature[3] == 4) ||
			(signature[2] == 5 && signature[3] == 6) ||
			(signature[2] == 7 && signature[3] == 8))
	if !isZIP {
		return nil, spreadsheetContainerError(signature)
	}

	remoteReader := &spreadsheetObjectReaderAt{source: source}
	reader, err := zip.NewReader(remoteReader, source.Size())
	if err != nil {
		if sourceErr := remoteReader.Error(); sourceErr != nil {
			return nil, sourceErr
		}
		return nil, apiError{
			Status:  http.StatusUnprocessableEntity,
			Code:    "invalid_workbook_package",
			Message: "the Office ZIP package is incomplete or its central directory cannot be read",
		}
	}
	return reader, nil
}

func openSpreadsheetReader(ctx context.Context, instance *storageInstance, key string, knownSize int64) (*zip.Reader, error) {
	source, err := openObjectRangeSource(ctx, instance, key)
	if err != nil {
		return nil, err
	}
	// The preview page passes the object size already returned by the folder
	// listing (or its one explicit HEAD request when opened directly). Reusing
	// that value avoids an extra separately billed storage request merely to
	// discover the ZIP length. The first real range response still verifies and
	// replaces the size through Content-Range.
	source.SetKnownSize(knownSize)
	if source.Size() <= 0 {
		if _, err := source.ReadSuffix(64 << 10); err != nil && err != io.EOF {
			return nil, err
		}
	}
	if source.Size() <= 0 {
		return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "unknown_workbook_size", Message: "spreadsheet preview requires a known non-zero object size"}
	}
	if source.Size() > maxSpreadsheetObjectBytes {
		return nil, apiError{Status: http.StatusRequestEntityTooLarge, Code: "workbook_too_large", Message: fmt.Sprintf("spreadsheet previews are limited to %d MiB", maxSpreadsheetObjectBytes>>20)}
	}
	return newSpreadsheetZipReader(source)
}

func (r *spreadsheetObjectReaderAt) ReadAt(destination []byte, offset int64) (int, error) {
	if len(destination) == 0 {
		return 0, nil
	}
	if offset < 0 || r == nil || r.source == nil {
		return 0, io.EOF
	}
	if r.source.Size() > 0 && offset >= r.source.Size() {
		return 0, io.EOF
	}

	written := 0
	for written < len(destination) {
		current := offset + int64(written)
		blockStart := current / spreadsheetRangeBlockBytes * spreadsheetRangeBlockBytes
		blockLength := spreadsheetRangeBlockBytes
		if r.source.Size() > 0 {
			blockLength = minInt64(blockLength, r.source.Size()-blockStart)
		}
		if blockLength <= 0 {
			break
		}
		if _, err := r.source.ReadRange(blockStart, blockLength); err != nil && err != io.EOF {
			r.rememberError(err)
			return written, err
		}
		remainingInBlock := blockStart + blockLength - current
		want := int64(len(destination) - written)
		if want > remainingInBlock {
			want = remainingInBlock
		}
		chunk, err := r.source.ReadRange(current, want)
		if err != nil && err != io.EOF {
			r.rememberError(err)
			return written, err
		}
		copied := copy(destination[written:], chunk)
		written += copied
		if int64(copied) < want {
			break
		}
	}
	if written < len(destination) {
		return written, io.EOF
	}
	return written, nil
}

func readWorkbookSheets(files map[string]*zip.File) ([]spreadsheetSheetInfo, error) {
	workbookFile := files["xl/workbook.xml"]
	relationshipFile := files["xl/_rels/workbook.xml.rels"]
	if workbookFile == nil || relationshipFile == nil {
		return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_workbook", Message: "the workbook is missing its worksheet index"}
	}
	var workbook workbookXML
	if err := decodeZipXML(workbookFile, &workbook); err != nil {
		return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_workbook", Message: "unable to read the workbook worksheet index"}
	}
	var relationships workbookRelationshipsXML
	if err := decodeZipXML(relationshipFile, &relationships); err != nil {
		return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_workbook", Message: "unable to read workbook relationships"}
	}
	targets := make(map[string]string)
	for _, relationship := range relationships.Relationships {
		target := strings.ReplaceAll(strings.TrimSpace(relationship.Target), "\\", "/")
		if target == "" || strings.Contains(target, ":") {
			continue
		}
		if strings.HasPrefix(target, "/") {
			target = strings.TrimLeft(target, "/")
		} else if !strings.HasPrefix(target, "xl/") {
			target = path.Join("xl", target)
		}
		target = path.Clean(target)
		if target == ".." || strings.HasPrefix(target, "../") {
			continue
		}
		targets[relationship.ID] = target
	}
	result := make([]spreadsheetSheetInfo, 0, len(workbook.Sheets))
	for index, sheet := range workbook.Sheets {
		target := targets[sheet.RelID]
		if target == "" || files[target] == nil {
			continue
		}
		hidden := strings.ToLower(strings.TrimSpace(sheet.State))
		if hidden != "hidden" && hidden != "veryhidden" {
			hidden = ""
		}
		result = append(result, spreadsheetSheetInfo{Index: index, Name: sheet.Name, Hidden: hidden, Target: target})
	}
	for index := range result {
		result[index].Index = index
	}
	return result, nil
}

func decodeZipXML(file *zip.File, destination any) error {
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	return xml.NewDecoder(io.LimitReader(reader, 32<<20)).Decode(destination)
}

func readSharedStrings(file *zip.File) ([]string, error) {
	if file == nil {
		return nil, nil
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	decoder := xml.NewDecoder(io.LimitReader(reader, maxSharedStringBytes+1))
	values := make([]string, 0)
	var consumed int
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_shared_strings", Message: "unable to read workbook shared strings"}
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "si" {
			continue
		}
		value, err := readRichText(decoder, start.Name.Local)
		if err != nil {
			return nil, err
		}
		consumed += len(value)
		if len(values) >= maxSharedStrings || consumed > maxSharedStringBytes {
			return nil, apiError{Status: http.StatusUnprocessableEntity, Code: "shared_strings_too_large", Message: "the workbook shared string table is too large to preview"}
		}
		values = append(values, value)
	}
	return values, nil
}

func readRichText(decoder *xml.Decoder, endLocal string) (string, error) {
	var builder strings.Builder
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return "", err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			depth++
			if typed.Name.Local == "t" {
				var text string
				if err := decoder.DecodeElement(&text, &typed); err != nil {
					return "", err
				}
				builder.WriteString(text)
				depth--
			}
		case xml.EndElement:
			depth--
			if depth == 0 && typed.Name.Local != endLocal {
				return "", fmt.Errorf("unexpected XML end element %q", typed.Name.Local)
			}
		}
	}
	return builder.String(), nil
}

func readSpreadsheetStyles(file *zip.File) (spreadsheetStyles, error) {
	styles := spreadsheetStyles{dateStyles: make(map[int]bool)}
	if file == nil {
		return styles, nil
	}
	var document workbookStylesXML
	if err := decodeZipXML(file, &document); err != nil {
		return styles, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_styles", Message: "unable to read workbook styles"}
	}
	custom := make(map[int]string)
	for _, format := range document.NumberFormats {
		custom[format.ID] = format.Code
	}
	for index, format := range document.CellFormats {
		styles.dateStyles[index] = isDateNumberFormat(format.NumberFormatID, custom[format.NumberFormatID])
	}
	return styles, nil
}

func isDateNumberFormat(id int, code string) bool {
	if (id >= 14 && id <= 22) || (id >= 27 && id <= 36) || (id >= 45 && id <= 47) || (id >= 50 && id <= 58) {
		return true
	}
	if code == "" {
		return false
	}
	var cleaned strings.Builder
	quoted := false
	bracketed := false
	for _, r := range strings.ToLower(code) {
		switch r {
		case '"':
			quoted = !quoted
		case '[':
			if !quoted {
				bracketed = true
			}
		case ']':
			if !quoted {
				bracketed = false
			}
		default:
			if !quoted && !bracketed && unicode.IsLetter(r) {
				cleaned.WriteRune(r)
			}
		}
	}
	letters := cleaned.String()
	return strings.ContainsAny(letters, "yd") || (strings.Contains(letters, "m") && strings.ContainsAny(letters, "hs"))
}

func readSpreadsheetSheet(ctx context.Context, file *zip.File, sheets []spreadsheetSheetInfo, sheetIndex int, sharedStrings []string, styles spreadsheetStyles, query spreadsheetQuery) (spreadsheetResponseJSON, error) {
	if file == nil {
		return spreadsheetResponseJSON{}, apiError{Status: http.StatusUnprocessableEntity, Code: "missing_sheet", Message: "the selected worksheet is missing from the workbook"}
	}
	reader, err := file.Open()
	if err != nil {
		return spreadsheetResponseJSON{}, err
	}
	defer reader.Close()
	// The sheet entry is the only part read without a decompressed-size cap
	// elsewhere; a tiny ZIP entry can inflate to gigabytes of XML.
	decoder := xml.NewDecoder(&hardLimitReader{reader: reader, limit: maxSpreadsheetSheetBytes + 1})
	state := &spreadsheetReadState{ctx: ctx}
	activeColumns := make(map[int]bool)
	pageRecords := make([]spreadsheetRecord, 0, query.pageSize)
	sortRecords := make([]spreadsheetRecord, 0)
	sourceRows := 0
	parsedRows := 0
	matchedRows := 0
	truncatedRows := false
	truncatedColumns := false
	pageStart := query.page * query.pageSize

	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return spreadsheetResponseJSON{}, spreadsheetParseError(err, ctx)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "row" {
			continue
		}
		// Rows without a visible cell are never counted as source rows, so they
		// need their own budget or a sheet of empty rows is parsed unbounded.
		parsedRows++
		if parsedRows > maxSpreadsheetRows {
			truncatedRows = true
			break
		}
		if err := state.tick(); err != nil {
			return spreadsheetResponseJSON{}, err
		}
		record, visible, beyondLimit, err := readSpreadsheetRow(decoder, start, sharedStrings, styles, state)
		if err != nil {
			return spreadsheetResponseJSON{}, spreadsheetParseError(err, ctx)
		}
		if beyondLimit {
			truncatedColumns = true
		}
		if !visible {
			continue
		}
		sourceRows++
		if sourceRows > maxSpreadsheetRows {
			truncatedRows = true
			break
		}
		for column, value := range record.cells {
			if strings.TrimSpace(value) != "" {
				activeColumns[column] = true
			}
		}
		if !spreadsheetRowMatches(record, query.filters, query.globalSearch) {
			continue
		}
		matchedRows++
		if query.sortColumn >= 0 {
			if len(sortRecords) >= maxSpreadsheetSortRows {
				return spreadsheetResponseJSON{}, apiError{Status: http.StatusUnprocessableEntity, Code: "sort_result_too_large", Message: fmt.Sprintf("sorting is limited to %d matching rows; add filters to narrow the result", maxSpreadsheetSortRows)}
			}
			sortRecords = append(sortRecords, record)
			continue
		}
		if matchedRows > pageStart && len(pageRecords) < query.pageSize {
			pageRecords = append(pageRecords, record)
		}
	}

	columns := make([]int, 0, len(activeColumns))
	for column := range activeColumns {
		columns = append(columns, column)
	}
	sort.Ints(columns)
	if len(columns) > maxSpreadsheetColumns {
		columns = columns[:maxSpreadsheetColumns]
		truncatedColumns = true
	}
	if query.sortColumn >= 0 {
		sort.SliceStable(sortRecords, func(i, j int) bool {
			comparison := compareSpreadsheetValues(sortRecords[i].cells[query.sortColumn], sortRecords[j].cells[query.sortColumn])
			if comparison == 0 {
				return sortRecords[i].rowNumber < sortRecords[j].rowNumber
			}
			if query.sortDirection == "desc" {
				return comparison > 0
			}
			return comparison < 0
		})
		start := minInt(pageStart, len(sortRecords))
		end := minInt(start+query.pageSize, len(sortRecords))
		pageRecords = append(pageRecords, sortRecords[start:end]...)
	}

	rows := make([]spreadsheetRowJSON, 0, len(pageRecords))
	pageBytes := 0
	for _, record := range pageRecords {
		cells := make([]string, len(columns))
		for index, column := range columns {
			cells[index] = record.cells[column]
			// Four bytes approximate the quotes, comma and escapes per cell, so
			// a page of empty cells is bounded as well.
			pageBytes += len(cells[index]) + 4
		}
		if pageBytes > maxSpreadsheetPageBytes {
			return spreadsheetResponseJSON{}, apiError{Status: http.StatusRequestEntityTooLarge, Code: "spreadsheet_page_too_large", Message: fmt.Sprintf("this page of the worksheet exceeds %d MiB; request a smaller page size", maxSpreadsheetPageBytes>>20)}
		}
		rows = append(rows, spreadsheetRowJSON{Number: record.rowNumber, Cells: cells})
	}
	headers := make([]string, len(columns))
	for index, column := range columns {
		headers[index] = excelColumnName(column)
	}
	return spreadsheetResponseJSON{
		Sheets:           sheets,
		Sheet:            sheets[sheetIndex],
		Headers:          headers,
		Rows:             rows,
		TotalRows:        matchedRows,
		SourceRows:       sourceRows,
		Page:             query.page,
		PageSize:         query.pageSize,
		TruncatedRows:    truncatedRows,
		TruncatedColumns: truncatedColumns,
		TruncatedCells:   state.truncatedCells,
	}, nil
}

// spreadsheetParseError maps a worksheet read failure to the API error shown to
// the client, keeping cancellation and the budget errors distinguishable from
// malformed XML.
func spreadsheetParseError(err error, ctx context.Context) error {
	var apiErr apiError
	switch {
	case errors.As(err, &apiErr):
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, errWordXMLLimit):
		return apiError{Status: http.StatusRequestEntityTooLarge, Code: "sheet_too_large", Message: fmt.Sprintf("the worksheet XML exceeds %d MiB once decompressed", maxSpreadsheetSheetBytes>>20)}
	}
	return apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_sheet", Message: "unable to parse the selected worksheet"}
}

func readSpreadsheetRow(decoder *xml.Decoder, start xml.StartElement, sharedStrings []string, styles spreadsheetStyles, state *spreadsheetReadState) (spreadsheetRecord, bool, bool, error) {
	rowNumber := 0
	for _, attribute := range start.Attr {
		if attribute.Name.Local == "r" {
			rowNumber, _ = strconv.Atoi(attribute.Value)
		}
	}
	cells := make(map[int]string)
	nextColumn := 0
	visible := false
	beyondLimit := false
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return spreadsheetRecord{}, false, false, err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			depth++
			if typed.Name.Local == "c" {
				if err := state.tick(); err != nil {
					return spreadsheetRecord{}, false, false, err
				}
				column, value, err := readSpreadsheetCell(decoder, typed, nextColumn, sharedStrings, styles, state)
				if err != nil {
					return spreadsheetRecord{}, false, false, err
				}
				depth--
				nextColumn = column + 1
				if strings.TrimSpace(value) != "" {
					visible = true
					if column >= maxSpreadsheetColumns {
						beyondLimit = true
					} else {
						cells[column] = value
					}
				} else if column < maxSpreadsheetColumns {
					cells[column] = ""
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	if rowNumber <= 0 {
		rowNumber = 1
	}
	return spreadsheetRecord{rowNumber: rowNumber, cells: cells}, visible, beyondLimit, nil
}

func readSpreadsheetCell(decoder *xml.Decoder, start xml.StartElement, fallbackColumn int, sharedStrings []string, styles spreadsheetStyles, state *spreadsheetReadState) (int, string, error) {
	column, value, err := readSpreadsheetCellValue(decoder, start, fallbackColumn, sharedStrings, styles, state)
	if err == nil && len(value) > maxSpreadsheetCellBytes {
		value = truncateUTF8Bytes(value, maxSpreadsheetCellBytes)
		state.truncatedCells = true
	}
	return column, value, err
}

func readSpreadsheetCellValue(decoder *xml.Decoder, start xml.StartElement, fallbackColumn int, sharedStrings []string, styles spreadsheetStyles, state *spreadsheetReadState) (int, string, error) {
	column := fallbackColumn
	cellType := ""
	styleIndex := -1
	for _, attribute := range start.Attr {
		switch attribute.Name.Local {
		case "r":
			if parsed, ok := excelColumnIndex(attribute.Value); ok {
				column = parsed
			}
		case "t":
			cellType = attribute.Value
		case "s":
			styleIndex, _ = strconv.Atoi(attribute.Value)
		}
	}
	var raw string
	var inline string
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return column, "", err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			depth++
			switch typed.Name.Local {
			case "v":
				if err := decoder.DecodeElement(&raw, &typed); err != nil {
					return column, "", err
				}
				depth--
			case "is":
				value, err := readRichText(decoder, typed.Name.Local)
				if err != nil {
					return column, "", err
				}
				inline = value
				depth--
			}
		case xml.EndElement:
			depth--
		}
	}
	raw = strings.TrimSpace(raw)
	switch cellType {
	case "s":
		index, err := strconv.Atoi(raw)
		if err == nil && index >= 0 && index < len(sharedStrings) {
			// Charge every reference, not each distinct entry: many cells can
			// alias one large string.
			state.referencedBytes += int64(minInt(len(sharedStrings[index]), maxSpreadsheetCellBytes))
			if state.referencedBytes > maxSpreadsheetReferencedBytes {
				return column, "", apiError{Status: http.StatusRequestEntityTooLarge, Code: "sheet_expansion_too_large", Message: "the worksheet references too much shared string data to preview"}
			}
			return column, sharedStrings[index], nil
		}
		return column, "", nil
	case "inlineStr":
		return column, inline, nil
	case "b":
		if raw == "1" {
			return column, "TRUE", nil
		}
		return column, "FALSE", nil
	case "d":
		return column, raw, nil
	case "e":
		if raw == "" {
			return column, "#ERROR", nil
		}
		return column, raw, nil
	}
	if styles.dateStyles[styleIndex] && raw != "" {
		if serial, err := strconv.ParseFloat(raw, 64); err == nil {
			return column, formatExcelSerialDate(serial), nil
		}
	}
	return column, raw, nil
}

func formatExcelSerialDate(serial float64) string {
	seconds := math.Round(serial * 24 * 60 * 60)
	value := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second)
	if math.Abs(serial-math.Round(serial)) < 1e-9 {
		return value.Format("2006-01-02")
	}
	return value.Format("2006-01-02T15:04:05Z")
}

func spreadsheetRowMatches(record spreadsheetRecord, filters map[int]string, globalSearch string) bool {
	if query := strings.ToLower(strings.TrimSpace(globalSearch)); query != "" {
		matched := false
		for _, value := range record.cells {
			if strings.Contains(strings.ToLower(value), query) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for column, filter := range filters {
		if !matchesSpreadsheetFilter(record.cells[column], filter) {
			return false
		}
	}
	return true
}

func matchesSpreadsheetFilter(value, rawFilter string) bool {
	filter := strings.TrimSpace(rawFilter)
	if filter == "" {
		return true
	}
	operator := "contains"
	operand := filter
	for _, candidate := range []string{">=", "<=", "!=", "=", ">", "<"} {
		if strings.HasPrefix(filter, candidate) {
			operator = candidate
			operand = strings.TrimSpace(strings.TrimPrefix(filter, candidate))
			break
		}
	}
	if operator == "contains" {
		return strings.Contains(strings.ToLower(value), strings.ToLower(operand))
	}
	comparison := compareSpreadsheetValues(value, operand)
	switch operator {
	case "=":
		return comparison == 0
	case "!=":
		return comparison != 0
	case ">":
		return comparison > 0
	case ">=":
		return comparison >= 0
	case "<":
		return comparison < 0
	case "<=":
		return comparison <= 0
	default:
		return true
	}
}

func compareSpreadsheetValues(left, right string) int {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" && right == "" {
		return 0
	}
	if left == "" {
		return 1
	}
	if right == "" {
		return -1
	}
	if leftNumber, err := strconv.ParseFloat(left, 64); err == nil {
		if rightNumber, err := strconv.ParseFloat(right, 64); err == nil {
			switch {
			case leftNumber < rightNumber:
				return -1
			case leftNumber > rightNumber:
				return 1
			default:
				return 0
			}
		}
	}
	if leftTime, err := time.Parse(time.RFC3339, left); err == nil {
		if rightTime, err := time.Parse(time.RFC3339, right); err == nil {
			if leftTime.Before(rightTime) {
				return -1
			}
			if leftTime.After(rightTime) {
				return 1
			}
			return 0
		}
	}
	return strings.Compare(strings.ToLower(left), strings.ToLower(right))
}

func excelColumnIndex(reference string) (int, bool) {
	value := strings.TrimSpace(reference)
	if value == "" {
		return 0, false
	}
	column := 0
	letters := 0
	for _, r := range value {
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		if r < 'A' || r > 'Z' {
			break
		}
		column = column*26 + int(r-'A'+1)
		letters++
	}
	if letters == 0 {
		return 0, false
	}
	return column - 1, true
}

func excelColumnName(index int) string {
	if index < 0 {
		return ""
	}
	var output []byte
	for index >= 0 {
		output = append(output, byte('A'+index%26))
		index = index/26 - 1
	}
	for left, right := 0, len(output)-1; left < right; left, right = left+1, right-1 {
		output[left], output[right] = output[right], output[left]
	}
	return string(output)
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
