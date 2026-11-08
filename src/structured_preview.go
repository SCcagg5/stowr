package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	maxStructuredPreviewBytes = int64(32 << 20)
	maxArchiveMetadataEntry   = int64(4 << 20)
)

type structuredPreviewResponse struct {
	Instance     string                 `json:"instance"`
	Key          string                 `json:"key"`
	Version      string                 `json:"version,omitempty"`
	Kind         string                 `json:"kind"`
	Container    string                 `json:"container,omitempty"`
	Properties   map[string]string      `json:"properties,omitempty"`
	Raw          string                 `json:"raw,omitempty"`
	RawEncoding  string                 `json:"rawEncoding,omitempty"`
	Contacts     []contactPreviewRecord `json:"contacts,omitempty"`
	Calendar     *calendarPreviewData   `json:"calendar,omitempty"`
	Email        *emailPreviewData      `json:"email,omitempty"`
	Certificates []map[string]string    `json:"certificates,omitempty"`
}

type contactPreviewRecord struct {
	Name         string   `json:"name,omitempty"`
	Structured   string   `json:"structuredName,omitempty"`
	Organization string   `json:"organization,omitempty"`
	Title        string   `json:"title,omitempty"`
	Emails       []string `json:"emails,omitempty"`
	Phones       []string `json:"phones,omitempty"`
	Addresses    []string `json:"addresses,omitempty"`
	URLs         []string `json:"urls,omitempty"`
	Notes        []string `json:"notes,omitempty"`
}

type calendarPreviewData struct {
	Name     string                  `json:"name,omitempty"`
	Timezone string                  `json:"timezone,omitempty"`
	Method   string                  `json:"method,omitempty"`
	Events   []calendarPreviewRecord `json:"events,omitempty"`
	Todos    []calendarPreviewRecord `json:"todos,omitempty"`
	FreeBusy []calendarPreviewRecord `json:"freeBusy,omitempty"`
	Journals []calendarPreviewRecord `json:"journals,omitempty"`
}

type calendarPreviewRecord struct {
	Type        string `json:"type,omitempty"`
	UID         string `json:"uid,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	Start       string `json:"start,omitempty"`
	End         string `json:"end,omitempty"`
	Due         string `json:"due,omitempty"`
	Status      string `json:"status,omitempty"`
	Organizer   string `json:"organizer,omitempty"`
	Recurrence  string `json:"recurrence,omitempty"`
}

type emailPreviewData struct {
	Headers     map[string]string        `json:"headers,omitempty"`
	BodyText    string                   `json:"bodyText,omitempty"`
	HTMLSource  string                   `json:"htmlSource,omitempty"`
	Attachments []emailPreviewAttachment `json:"attachments,omitempty"`
}

type emailPreviewAttachment struct {
	Name        string `json:"name,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Inline      bool   `json:"inline,omitempty"`
	ContentID   string `json:"contentId,omitempty"`
}

type archivePreviewResponse struct {
	Instance   string                `json:"instance"`
	Key        string                `json:"key"`
	Version    string                `json:"version,omitempty"`
	Container  string                `json:"container"`
	Properties map[string]string     `json:"properties,omitempty"`
	Package    map[string]string     `json:"package,omitempty"`
	Entries    []archivePreviewEntry `json:"entries"`
	EPUB       *epubPreviewData      `json:"epub,omitempty"`
}

type archivePreviewEntry struct {
	Name             string `json:"name"`
	CompressedSize   uint64 `json:"compressedSize"`
	UncompressedSize uint64 `json:"uncompressedSize"`
	Method           string `json:"method"`
	Modified         string `json:"modified,omitempty"`
	Encrypted        bool   `json:"encrypted,omitempty"`
}

type epubPreviewData struct {
	Title       string         `json:"title,omitempty"`
	Creators    []string       `json:"creators,omitempty"`
	Language    string         `json:"language,omitempty"`
	Identifier  string         `json:"identifier,omitempty"`
	Publisher   string         `json:"publisher,omitempty"`
	Date        string         `json:"date,omitempty"`
	Rights      string         `json:"rights,omitempty"`
	Description string         `json:"description,omitempty"`
	Subjects    []string       `json:"subjects,omitempty"`
	Cover       string         `json:"cover,omitempty"`
	TOC         []epubTOCEntry `json:"toc,omitempty"`
}

type epubTOCEntry struct {
	Label string `json:"label"`
	Href  string `json:"href,omitempty"`
	Depth int    `json:"depth,omitempty"`
}

func (a *application) handleStructuredPreview(w http.ResponseWriter, r *http.Request) {
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
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key cannot be empty"})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}
	extension := strings.ToLower(strings.TrimPrefix(path.Ext(key), "."))
	contentType := strings.TrimSpace(r.URL.Query().Get("mime"))
	listed := mediaSourceMetadata{
		Size:         parseInt64Default(r.URL.Query().Get("size"), 0),
		MIME:         contentType,
		ETag:         strings.TrimSpace(r.URL.Query().Get("etag")),
		LastModified: strings.TrimSpace(r.URL.Query().Get("lastModified")),
	}
	kind := ""
	switch {
	case isContactExtension(extension, contentType):
		kind = "contact"
	case isCalendarExtension(extension, contentType):
		kind = "calendar"
	case isEmailExtension(extension, contentType):
		kind = "email"
	case isCertificateExtension(extension, contentType):
		kind = "certificate"
	default:
		writeAPIError(w, apiError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_preview", Message: "this object does not have a structured preview"})
		return
	}

	data, source, err := readCompleteStructuredObject(r.Context(), instance, key, listed, maxStructuredPreviewBytes)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	response := structuredPreviewResponse{
		Instance:    instance.cfg.ID,
		Key:         key,
		Version:     strings.TrimSpace(r.URL.Query().Get("version")),
		Kind:        kind,
		RawEncoding: "text",
	}
	switch kind {
	case "contact":
		response.Container = contactContainerLabel(extension)
		response.Properties = parseVCardMetadata(data)
		response.Contacts = parseVCardPreview(data)
		response.Raw = string(data)
	case "calendar":
		response.Container = calendarContainerLabel(extension)
		response.Properties = parseICalendarMetadata(data)
		response.Calendar = parseICalendarPreview(data)
		response.Raw = string(data)
	case "email":
		response.Container = emailContainerLabel(extension)
		headers, _ := parseEmailHeaders(data)
		response.Properties = emailProperties(headers)
		response.Email = parseEmailPreview(data)
		response.Raw = string(data)
	case "certificate":
		response.Container = certificateContainerLabel(extension, contentType)
		response.Properties = parseCertificateProperties(data, extension)
		response.Certificates = parseAllCertificateProperties(data, extension)
		if isMostlyText(data) {
			response.Raw = string(data)
		} else {
			response.RawEncoding = "base64"
			response.Raw = base64.StdEncoding.EncodeToString(data)
		}
	}
	if source != nil && response.Properties == nil {
		response.Properties = map[string]string{}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *application) handleArchivePreview(w http.ResponseWriter, r *http.Request) {
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
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "invalid_key", Message: "object key cannot be empty"})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}
	extension := strings.ToLower(strings.TrimPrefix(path.Ext(key), "."))
	if !isZipArchiveExtension(extension) {
		writeAPIError(w, apiError{Status: http.StatusUnsupportedMediaType, Code: "unsupported_preview", Message: "this archive format does not expose a deterministic central directory preview"})
		return
	}
	listed := mediaSourceMetadata{
		Size:         parseInt64Default(r.URL.Query().Get("size"), 0),
		ETag:         strings.TrimSpace(r.URL.Query().Get("etag")),
		LastModified: strings.TrimSpace(r.URL.Query().Get("lastModified")),
	}
	source, err := openStructuredRangeSource(r.Context(), instance, key, listed)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	reader, err := openRemoteZIPReader(source, extension)
	if err != nil {
		writeAPIError(w, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_zip", Message: "the object does not contain a readable ZIP central directory"})
		return
	}
	entryLimit := a.config.Runtime.MaxArchiveEntries
	if entryLimit <= 0 {
		entryLimit = defaultMaxArchiveEntries
	}
	if len(reader.File) > entryLimit {
		writeAPIError(w, apiError{Status: http.StatusRequestEntityTooLarge, Code: "archive_too_many_entries", Message: fmt.Sprintf("the archive contains %d entries; the configured complete preview limit is %d", len(reader.File), entryLimit)})
		return
	}
	response := archivePreviewResponse{
		Instance:   instance.cfg.ID,
		Key:        key,
		Version:    strings.TrimSpace(r.URL.Query().Get("version")),
		Container:  archiveContainerLabel(extension),
		Properties: zipArchiveProperties(reader),
		Entries:    make([]archivePreviewEntry, 0, len(reader.File)),
	}
	for _, file := range reader.File {
		// The archive inventory is intentionally a file list, not a synthetic
		// filesystem tree. Explicit directory records and symbolic links are not
		// actionable preview targets, so omitting them keeps the response smaller
		// and guarantees that every visible row uses the same interaction model.
		if file.FileInfo().IsDir() || strings.HasSuffix(file.Name, "/") || file.Mode()&fs.ModeSymlink != 0 || !file.Mode().IsRegular() {
			continue
		}
		modified := ""
		if !file.Modified.IsZero() {
			modified = file.Modified.UTC().Format(time.RFC3339)
		}
		response.Entries = append(response.Entries, archivePreviewEntry{
			Name:             file.Name,
			CompressedSize:   file.CompressedSize64,
			UncompressedSize: file.UncompressedSize64,
			Method:           zipCompressionMethodLabel(file.Method),
			Modified:         modified,
			Encrypted:        file.Flags&0x1 != 0,
		})
	}
	response.Package = inspectArchivePackageMetadata(reader, extension)
	if extension == "epub" {
		response.EPUB = inspectEPUBPreview(reader)
	}
	writeJSON(w, http.StatusOK, response)
}

func readCompleteStructuredObject(ctx context.Context, instance *storageInstance, key string, listed mediaSourceMetadata, maximum int64) ([]byte, *objectRangeSource, error) {
	source, err := openStructuredRangeSource(ctx, instance, key, listed)
	if err != nil {
		return nil, nil, err
	}
	if source.Size() <= 0 {
		if _, err := source.ReadSuffix(1); err != nil && err != io.EOF {
			return nil, source, err
		}
	}
	size := source.Size()
	if size <= 0 {
		return nil, source, apiError{Status: http.StatusUnprocessableEntity, Code: "unknown_size", Message: "the object size could not be determined safely"}
	}
	if size > maximum {
		return nil, source, apiError{Status: http.StatusRequestEntityTooLarge, Code: "preview_too_large", Message: fmt.Sprintf("this structured preview reads the complete object and is limited to %s", formatByteSize(maximum))}
	}
	data, err := source.ReadRange(0, size)
	if err != nil && err != io.EOF {
		return nil, source, err
	}
	if int64(len(data)) != size {
		return nil, source, apiError{Status: http.StatusBadGateway, Code: "preview_truncated", Message: "the storage provider returned a truncated object"}
	}
	return data, source, nil
}

func formatByteSize(value int64) string {
	const unit = int64(1024)
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	labels := []string{"KB", "MB", "GB", "TB"}
	amount := float64(value)
	for _, label := range labels {
		amount /= 1024
		if amount < 1024 {
			return fmt.Sprintf("%.0f %s", amount, label)
		}
	}
	return fmt.Sprintf("%d B", value)
}

func isMostlyText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	printable := 0
	for _, value := range data {
		if value == 0 {
			return false
		}
		if value == '\n' || value == '\r' || value == '\t' || (value >= 0x20 && value < 0x7f) || value >= 0xc2 {
			printable++
		}
	}
	return printable*100/len(data) >= 90
}

// maxContactValuesPerField bounds distinct EMAIL/TEL/ADR/URL/NOTE values kept per
// contact so a crafted card cannot inflate the preview or the dedupe work.
const maxContactValuesPerField = 256

// appendUniqueLimited is appendUniqueString with a map-backed membership check
// and a cap on the slice length, keeping dedupe linear in the number of values.
func appendUniqueLimited(values []string, seen map[string]struct{}, field, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || len(values) >= maxContactValuesPerField {
		return values
	}
	key := field + "\x00" + value
	if _, ok := seen[key]; ok {
		return values
	}
	seen[key] = struct{}{}
	return append(values, value)
}

func parseVCardPreview(data []byte) []contactPreviewRecord {
	blocks := splitICalBlocks(unfoldStructuredLines(string(data)), "BEGIN:VCARD", "END:VCARD")
	result := make([]contactPreviewRecord, 0, len(blocks))
	for _, block := range blocks {
		record := contactPreviewRecord{}
		seen := make(map[string]struct{})
		for _, line := range block {
			name := structuredPropertyName(line)
			value := decodeStructuredValue(structuredPropertyValue(line))
			if value == "" {
				continue
			}
			switch name {
			case "FN":
				record.Name = value
			case "N":
				record.Structured = strings.ReplaceAll(value, ";", " ")
			case "ORG":
				record.Organization = strings.ReplaceAll(value, ";", " · ")
			case "TITLE", "ROLE":
				if record.Title == "" {
					record.Title = value
				}
			case "EMAIL":
				record.Emails = appendUniqueLimited(record.Emails, seen, "EMAIL", value)
			case "TEL":
				record.Phones = appendUniqueLimited(record.Phones, seen, "TEL", value)
			case "ADR":
				record.Addresses = appendUniqueLimited(record.Addresses, seen, "ADR", normalizeStructuredAddress(value))
			case "URL":
				record.URLs = appendUniqueLimited(record.URLs, seen, "URL", value)
			case "NOTE":
				record.Notes = appendUniqueLimited(record.Notes, seen, "NOTE", value)
			}
		}
		if record.Name == "" {
			record.Name = strings.TrimSpace(record.Structured)
		}
		result = append(result, record)
	}
	return result
}

func decodeStructuredValue(value string) string {
	replacer := strings.NewReplacer("\\n", "\n", "\\N", "\n", "\\,", ",", "\\;", ";", "\\\\", "\\")
	return strings.TrimSpace(replacer.Replace(value))
}

func normalizeStructuredAddress(value string) string {
	parts := strings.Split(value, ";")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, ", ")
}

func parseICalendarPreview(data []byte) *calendarPreviewData {
	lines := unfoldStructuredLines(string(data))
	calendar := &calendarPreviewData{}
	calendar.Name = firstStructuredValue(lines, "X-WR-CALNAME")
	calendar.Timezone = firstStructuredValue(lines, "X-WR-TIMEZONE")
	calendar.Method = firstStructuredValue(lines, "METHOD")
	for _, spec := range []struct {
		begin string
		end   string
		typ   string
		dest  *[]calendarPreviewRecord
	}{
		{"BEGIN:VEVENT", "END:VEVENT", "Event", &calendar.Events},
		{"BEGIN:VTODO", "END:VTODO", "Todo", &calendar.Todos},
		{"BEGIN:VFREEBUSY", "END:VFREEBUSY", "Free/busy", &calendar.FreeBusy},
		{"BEGIN:VJOURNAL", "END:VJOURNAL", "Journal", &calendar.Journals},
	} {
		for _, block := range splitICalBlocks(lines, spec.begin, spec.end) {
			record := calendarPreviewRecord{
				Type:        spec.typ,
				UID:         firstStructuredValue(block, "UID"),
				Summary:     firstStructuredValue(block, "SUMMARY"),
				Description: decodeStructuredValue(firstStructuredValue(block, "DESCRIPTION")),
				Location:    decodeStructuredValue(firstStructuredValue(block, "LOCATION")),
				Start:       firstStructuredValue(block, "DTSTART"),
				End:         firstStructuredValue(block, "DTEND"),
				Due:         firstStructuredValue(block, "DUE"),
				Status:      firstStructuredValue(block, "STATUS"),
				Organizer:   firstStructuredValue(block, "ORGANIZER"),
				Recurrence:  firstStructuredValue(block, "RRULE"),
			}
			*spec.dest = append(*spec.dest, record)
		}
	}
	return calendar
}

func firstStructuredValue(lines []string, property string) string {
	property = strings.ToUpper(property)
	for _, line := range lines {
		if structuredPropertyName(line) == property {
			return decodeStructuredValue(structuredPropertyValue(line))
		}
	}
	return ""
}

func parseEmailPreview(data []byte) *emailPreviewData {
	message, err := mail.ReadMessage(bytes.NewReader(data))
	if err != nil {
		headers, _ := parseEmailHeaders(data)
		return &emailPreviewData{Headers: displayEmailHeaders(headers)}
	}
	preview := &emailPreviewData{Headers: make(map[string]string)}
	for _, name := range []string{"From", "To", "Cc", "Bcc", "Reply-To", "Subject", "Date", "Message-ID", "In-Reply-To", "References", "Content-Type", "MIME-Version", "User-Agent", "X-Mailer"} {
		if value := strings.TrimSpace(message.Header.Get(name)); value != "" {
			preview.Headers[name] = value
		}
	}
	body, _ := io.ReadAll(message.Body)
	walkMIMEPreview(textprotoHeaderFromMail(message.Header), body, preview)
	preview.BodyText = strings.TrimSpace(preview.BodyText)
	preview.HTMLSource = strings.TrimSpace(preview.HTMLSource)
	if preview.BodyText == "" && preview.HTMLSource != "" {
		preview.BodyText = htmlToPlainText(preview.HTMLSource)
	}
	return preview
}

// mail.Header and multipart.Part.Header are both thin MIME header maps but use
// different named types. Keep conversion local to the preview parser.
func textprotoHeaderFromMail(header mail.Header) map[string][]string {
	out := make(map[string][]string, len(header))
	for key, values := range header {
		out[key] = append([]string(nil), values...)
	}
	return out
}

const (
	maxMIMEPreviewDepth = 8
	maxMIMEPreviewParts = 4096
)

func walkMIMEPreview(header map[string][]string, body []byte, preview *emailPreviewData) {
	walker := &mimeWalker{preview: preview}
	walker.walk(header, bytes.NewReader(body), 0)
}

// mimeWalker visits MIME parts with a depth and part budget. Nested multipart
// bodies are streamed from the parent part instead of being copied per level,
// so work and memory stay linear in the message size.
type mimeWalker struct {
	preview *emailPreviewData
	parts   int
}

func (w *mimeWalker) walk(header map[string][]string, reader io.Reader, depth int) {
	preview := w.preview
	contentType := firstHeaderValue(header, "Content-Type")
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType == "" {
		mediaType = "text/plain"
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		boundary := params["boundary"]
		if boundary == "" || depth >= maxMIMEPreviewDepth {
			return
		}
		if depth > 0 {
			switch strings.ToLower(strings.TrimSpace(firstHeaderValue(header, "Content-Transfer-Encoding"))) {
			case "base64", "quoted-printable":
				raw, readErr := io.ReadAll(reader)
				if readErr != nil {
					return
				}
				reader = bytes.NewReader(decodeTransferEncoding(header, raw))
			}
		}
		multipartReader := multipart.NewReader(reader, boundary)
		for w.parts < maxMIMEPreviewParts {
			part, err := multipartReader.NextPart()
			if err != nil {
				break
			}
			w.parts++
			partHeader := make(map[string][]string, len(part.Header))
			for key, values := range part.Header {
				partHeader[key] = append([]string(nil), values...)
			}
			w.walk(partHeader, part, depth+1)
			_ = part.Close()
		}
		return
	}
	body, readErr := io.ReadAll(reader)
	if readErr != nil {
		return
	}
	body = decodeTransferEncoding(header, body)
	disposition, dispositionParams, _ := mime.ParseMediaType(firstHeaderValue(header, "Content-Disposition"))
	filename := strings.TrimSpace(dispositionParams["filename"])
	if filename == "" {
		_, typeParams, _ := mime.ParseMediaType(contentType)
		filename = strings.TrimSpace(typeParams["name"])
	}
	contentID := strings.Trim(strings.TrimSpace(firstHeaderValue(header, "Content-ID")), "<>")
	isAttachment := strings.EqualFold(disposition, "attachment") || filename != ""
	if isAttachment {
		preview.Attachments = append(preview.Attachments, emailPreviewAttachment{
			Name:        filename,
			ContentType: mediaType,
			Size:        int64(len(body)),
			Inline:      strings.EqualFold(disposition, "inline"),
			ContentID:   contentID,
		})
		return
	}
	switch strings.ToLower(mediaType) {
	case "text/plain":
		if preview.BodyText == "" {
			preview.BodyText = decodeTextBody(body, params["charset"])
		}
	case "text/html":
		if preview.HTMLSource == "" {
			preview.HTMLSource = decodeTextBody(body, params["charset"])
		}
	default:
		if contentID != "" || strings.EqualFold(disposition, "inline") {
			preview.Attachments = append(preview.Attachments, emailPreviewAttachment{
				Name: filename, ContentType: mediaType, Size: int64(len(body)), Inline: true, ContentID: contentID,
			})
		}
	}
}

func firstHeaderValue(header map[string][]string, name string) string {
	for key, values := range header {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func decodeTransferEncoding(header map[string][]string, data []byte) []byte {
	encoding := strings.ToLower(strings.TrimSpace(firstHeaderValue(header, "Content-Transfer-Encoding")))
	switch encoding {
	case "base64":
		cleaned := strings.Map(func(r rune) rune {
			if r == '\r' || r == '\n' || r == ' ' || r == '\t' {
				return -1
			}
			return r
		}, string(data))
		decoded, err := base64.StdEncoding.DecodeString(cleaned)
		if err == nil {
			return decoded
		}
	case "quoted-printable":
		decoded, err := io.ReadAll(newQuotedPrintableReader(bytes.NewReader(data)))
		if err == nil {
			return decoded
		}
	}
	return data
}

func newQuotedPrintableReader(reader io.Reader) io.Reader {
	return quotedprintable.NewReader(reader)
}

func decodeTextBody(data []byte, charset string) string {
	// UTF-8 and ASCII are safe to expose directly. Other charsets remain byte
	// preserving rather than invoking an external transcoder.
	_ = charset
	return string(bytes.ToValidUTF8(data, []byte("�")))
}

var (
	htmlTagPattern       = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlScriptPattern    = regexp.MustCompile(`(?is)<script[^>]*>.*?</script\s*>`)
	htmlStylePattern     = regexp.MustCompile(`(?is)<style[^>]*>.*?</style\s*>`)
	htmlLineBreakPattern = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</li>|</tr>`)
)

func htmlToPlainText(value string) string {
	value = htmlScriptPattern.ReplaceAllString(value, " ")
	value = htmlStylePattern.ReplaceAllString(value, " ")
	value = htmlLineBreakPattern.ReplaceAllString(value, "\n")
	value = htmlTagPattern.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	lines := strings.Split(strings.ReplaceAll(value, "\r", ""), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func displayEmailHeaders(headers map[string]string) map[string]string {
	result := make(map[string]string)
	for key, value := range headers {
		result[http.CanonicalHeaderKey(key)] = value
	}
	return result
}

func parseAllCertificateProperties(data []byte, extension string) []map[string]string {
	blocks := parsePEMBlocks(data)
	result := make([]map[string]string, 0, maxInt(1, len(blocks)))
	if len(blocks) == 0 {
		props := make(map[string]string)
		if extension == "csr" || extension == "req" {
			mergeCertificateBlockProperties(props, "CERTIFICATE REQUEST", data)
		} else {
			mergeCertificateBlockProperties(props, "CERTIFICATE", data)
		}
		if len(props) > 0 {
			result = append(result, props)
		}
		return result
	}
	for _, block := range blocks {
		props := make(map[string]string)
		mergeCertificateBlockProperties(props, block.Type, block.Bytes)
		if len(props) == 0 {
			props["Block"] = block.Type
		}
		result = append(result, props)
	}
	return result
}

func openRemoteZIPReader(source *objectRangeSource, extension string) (*zip.Reader, error) {
	if source.Size() <= 0 {
		if _, err := source.ReadSuffix(128 << 10); err != nil && err != io.EOF {
			return nil, err
		}
	}
	size := source.Size()
	if size <= 0 {
		return nil, fmt.Errorf("unknown object size")
	}
	base := io.ReaderAt(&spreadsheetObjectReaderAt{source: source})
	offset := int64(0)
	if extension == "crx" {
		headerLength := minInt64(size, 64<<10)
		header, err := source.ReadRange(0, headerLength)
		if err != nil && err != io.EOF {
			return nil, err
		}
		offset, err = crxZIPOffset(header, size)
		if err != nil {
			return nil, err
		}
	}
	if offset < 0 || offset >= size {
		return nil, fmt.Errorf("invalid ZIP offset")
	}
	if offset > 0 {
		base = io.NewSectionReader(base, offset, size-offset)
	}
	return zip.NewReader(base, size-offset)
}

func crxZIPOffset(header []byte, size int64) (int64, error) {
	if len(header) < 12 || string(header[:4]) != "Cr24" {
		return 0, fmt.Errorf("invalid CRX header")
	}
	version := binary.LittleEndian.Uint32(header[4:8])
	switch version {
	case 2:
		if len(header) < 16 {
			return 0, fmt.Errorf("truncated CRX2 header")
		}
		publicKeyLength := int64(binary.LittleEndian.Uint32(header[8:12]))
		signatureLength := int64(binary.LittleEndian.Uint32(header[12:16]))
		offset := int64(16) + publicKeyLength + signatureLength
		if offset >= size {
			return 0, fmt.Errorf("invalid CRX2 payload offset")
		}
		return offset, nil
	case 3:
		headerSize := int64(binary.LittleEndian.Uint32(header[8:12]))
		offset := int64(12) + headerSize
		if offset >= size {
			return 0, fmt.Errorf("invalid CRX3 payload offset")
		}
		return offset, nil
	default:
		return 0, fmt.Errorf("unsupported CRX version %d", version)
	}
}

func inspectArchivePackageMetadata(reader *zip.Reader, extension string) map[string]string {
	switch extension {
	case "jar", "war", "ear", "aar":
		return parseManifestMetadata(readZIPEntry(reader, "META-INF/MANIFEST.MF", maxArchiveMetadataEntry))
	case "xpi":
		return parseJSONManifestMetadata(readZIPEntry(reader, "manifest.json", maxArchiveMetadataEntry))
	case "vsix":
		return parseVSIXMetadata(readZIPEntry(reader, "extension.vsixmanifest", maxArchiveMetadataEntry))
	case "apk":
		return parseAPKPackageMetadata(reader)
	case "crx":
		return parseJSONManifestMetadata(readZIPEntry(reader, "manifest.json", maxArchiveMetadataEntry))
	default:
		return nil
	}
}

func readZIPEntry(reader *zip.Reader, name string, maximum int64) []byte {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	for _, file := range reader.File {
		clean := strings.TrimPrefix(path.Clean("/"+file.Name), "/")
		if !strings.EqualFold(clean, name) || file.FileInfo().IsDir() || int64(file.UncompressedSize64) > maximum {
			continue
		}
		entry, err := file.Open()
		if err != nil {
			return nil
		}
		data, err := io.ReadAll(io.LimitReader(entry, maximum+1))
		_ = entry.Close()
		if err != nil || int64(len(data)) > maximum {
			return nil
		}
		return data
	}
	return nil
}

func parseManifestMetadata(data []byte) map[string]string {
	if len(data) == 0 {
		return nil
	}
	lines := unfoldStructuredLines(string(data))
	allowed := map[string]string{
		"Manifest-Version": "Manifest version", "Main-Class": "Main class", "Automatic-Module-Name": "Module",
		"Implementation-Title": "Implementation", "Implementation-Version": "Version", "Implementation-Vendor": "Vendor",
		"Specification-Title": "Specification", "Specification-Version": "Specification version", "Specification-Vendor": "Specification vendor",
		"Bundle-Name": "Bundle", "Bundle-SymbolicName": "Bundle ID", "Bundle-Version": "Bundle version", "Created-By": "Created by",
	}
	props := make(map[string]string)
	for _, line := range lines {
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(line[:idx])
		if label := allowed[name]; label != "" {
			props[label] = strings.TrimSpace(line[idx+1:])
		}
	}
	return props
}

func parseJSONManifestMetadata(data []byte) map[string]string {
	if len(data) == 0 {
		return nil
	}
	var manifest map[string]any
	if json.Unmarshal(data, &manifest) != nil {
		return nil
	}
	props := make(map[string]string)
	for key, label := range map[string]string{
		"name": "Name", "short_name": "Short name", "version": "Version", "version_name": "Version name",
		"description": "Description", "author": "Author", "homepage_url": "Homepage", "manifest_version": "Manifest version",
		"minimum_chrome_version": "Minimum browser version", "default_locale": "Default locale",
	} {
		if value, exists := manifest[key]; exists {
			props[label] = fmt.Sprint(value)
		}
	}
	if permissions, ok := manifest["permissions"].([]any); ok {
		values := make([]string, 0, len(permissions))
		for _, permission := range permissions {
			values = append(values, fmt.Sprint(permission))
		}
		if len(values) > 0 {
			props["Permissions"] = strings.Join(values, ", ")
		}
	}
	return props
}

type vsixManifestXML struct {
	Identity struct {
		ID        string `xml:"Id,attr"`
		Version   string `xml:"Version,attr"`
		Publisher string `xml:"Publisher,attr"`
		Language  string `xml:"Language,attr"`
	} `xml:"Metadata>Identity"`
	DisplayName string `xml:"Metadata>DisplayName"`
	Description string `xml:"Metadata>Description"`
	MoreInfo    string `xml:"Metadata>MoreInfo"`
	License     string `xml:"Metadata>License"`
}

func parseVSIXMetadata(data []byte) map[string]string {
	if len(data) == 0 {
		return nil
	}
	var manifest vsixManifestXML
	if xml.Unmarshal(data, &manifest) != nil {
		return nil
	}
	props := make(map[string]string)
	for label, value := range map[string]string{
		"ID": manifest.Identity.ID, "Version": manifest.Identity.Version, "Publisher": manifest.Identity.Publisher,
		"Language": manifest.Identity.Language, "Name": manifest.DisplayName, "Description": manifest.Description,
		"More information": manifest.MoreInfo, "License": manifest.License,
	} {
		if strings.TrimSpace(value) != "" {
			props[label] = strings.TrimSpace(value)
		}
	}
	return props
}

func parseAPKPackageMetadata(reader *zip.Reader) map[string]string {
	props := make(map[string]string)
	abis := make(map[string]struct{})
	dexFiles := 0
	signatures := 0
	resources := false
	manifest := false
	for _, file := range reader.File {
		name := strings.TrimPrefix(path.Clean("/"+file.Name), "/")
		lower := strings.ToLower(name)
		if lower == "androidmanifest.xml" {
			manifest = true
		}
		if lower == "resources.arsc" {
			resources = true
		}
		if strings.HasPrefix(lower, "classes") && strings.HasSuffix(lower, ".dex") {
			dexFiles++
		}
		if strings.HasPrefix(lower, "lib/") {
			parts := strings.Split(name, "/")
			if len(parts) > 2 && parts[1] != "" {
				abis[parts[1]] = struct{}{}
			}
		}
		if strings.HasPrefix(strings.ToUpper(name), "META-INF/") && (strings.HasSuffix(strings.ToUpper(name), ".RSA") || strings.HasSuffix(strings.ToUpper(name), ".DSA") || strings.HasSuffix(strings.ToUpper(name), ".EC")) {
			signatures++
		}
	}
	if manifest {
		props["Android manifest"] = "Present"
	}
	if resources {
		props["Compiled resources"] = "Present"
	}
	if dexFiles > 0 {
		props["DEX files"] = strconv.Itoa(dexFiles)
	}
	if signatures > 0 {
		props["Signature blocks"] = strconv.Itoa(signatures)
	}
	if len(abis) > 0 {
		values := make([]string, 0, len(abis))
		for abi := range abis {
			values = append(values, abi)
		}
		sort.Strings(values)
		props["Native ABIs"] = strings.Join(values, ", ")
	}
	return props
}

func inspectEPUBPreview(reader *zip.Reader) *epubPreviewData {
	containerData := readZIPEntry(reader, "META-INF/container.xml", maxArchiveMetadataEntry)
	if len(containerData) == 0 {
		return nil
	}
	var container struct {
		Rootfiles []struct {
			FullPath  string `xml:"full-path,attr"`
			MediaType string `xml:"media-type,attr"`
		} `xml:"rootfiles>rootfile"`
	}
	if xml.Unmarshal(containerData, &container) != nil || len(container.Rootfiles) == 0 {
		return nil
	}
	opfPath := strings.TrimPrefix(path.Clean("/"+container.Rootfiles[0].FullPath), "/")
	opfData := readZIPEntry(reader, opfPath, maxArchiveMetadataEntry)
	if len(opfData) == 0 {
		return nil
	}
	var pkg struct {
		Metadata struct {
			Titles       []string `xml:"title"`
			Creators     []string `xml:"creator"`
			Languages    []string `xml:"language"`
			Identifiers  []string `xml:"identifier"`
			Publishers   []string `xml:"publisher"`
			Dates        []string `xml:"date"`
			Rights       []string `xml:"rights"`
			Descriptions []string `xml:"description"`
			Subjects     []string `xml:"subject"`
			Meta         []struct {
				Name     string `xml:"name,attr"`
				Content  string `xml:"content,attr"`
				Property string `xml:"property,attr"`
				Value    string `xml:",chardata"`
			} `xml:"meta"`
		} `xml:"metadata"`
		Manifest []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"manifest>item"`
		Spine struct {
			TOC string `xml:"toc,attr"`
		} `xml:"spine"`
	}
	if xml.Unmarshal(opfData, &pkg) != nil {
		return nil
	}
	first := func(values []string) string {
		if len(values) == 0 {
			return ""
		}
		return strings.TrimSpace(values[0])
	}
	result := &epubPreviewData{
		Title: first(pkg.Metadata.Titles), Creators: trimNonEmpty(pkg.Metadata.Creators), Language: first(pkg.Metadata.Languages),
		Identifier: first(pkg.Metadata.Identifiers), Publisher: first(pkg.Metadata.Publishers), Date: first(pkg.Metadata.Dates),
		Rights: first(pkg.Metadata.Rights), Description: first(pkg.Metadata.Descriptions), Subjects: trimNonEmpty(pkg.Metadata.Subjects),
	}
	baseDir := path.Dir(opfPath)
	manifestByID := make(map[string]struct {
		Href, MediaType, Properties string
	})
	navPath := ""
	ncxPath := ""
	for _, item := range pkg.Manifest {
		manifestByID[item.ID] = struct{ Href, MediaType, Properties string }{item.Href, item.MediaType, item.Properties}
		properties := strings.Fields(item.Properties)
		for _, property := range properties {
			if property == "nav" {
				navPath = resolveArchiveRelativePath(baseDir, item.Href)
			}
			if property == "cover-image" {
				result.Cover = resolveArchiveRelativePath(baseDir, item.Href)
			}
		}
	}
	for _, meta := range pkg.Metadata.Meta {
		if strings.EqualFold(meta.Name, "cover") {
			if item, ok := manifestByID[meta.Content]; ok {
				result.Cover = resolveArchiveRelativePath(baseDir, item.Href)
			}
		}
	}
	if item, ok := manifestByID[pkg.Spine.TOC]; ok {
		ncxPath = resolveArchiveRelativePath(baseDir, item.Href)
	}
	if navPath != "" {
		result.TOC = parseEPUBNav(readZIPEntry(reader, navPath, maxArchiveMetadataEntry))
	}
	if len(result.TOC) == 0 && ncxPath != "" {
		result.TOC = parseEPUBNCX(readZIPEntry(reader, ncxPath, maxArchiveMetadataEntry))
	}
	return result
}

func trimNonEmpty(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func resolveArchiveRelativePath(baseDir, relative string) string {
	return strings.TrimPrefix(path.Clean("/"+path.Join(baseDir, relative)), "/")
}

func parseEPUBNav(data []byte) []epubTOCEntry {
	if len(data) == 0 {
		return nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	result := make([]epubTOCEntry, 0)
	depth := 0
	inNav := false
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch value := token.(type) {
		case xml.StartElement:
			local := strings.ToLower(value.Name.Local)
			if local == "nav" {
				inNav = true
			}
			if inNav && (local == "ol" || local == "ul") {
				depth++
			}
			if inNav && local == "a" {
				href := ""
				for _, attr := range value.Attr {
					if strings.EqualFold(attr.Name.Local, "href") {
						href = attr.Value
					}
				}
				var label string
				if decoder.DecodeElement(&label, &value) == nil {
					label = strings.Join(strings.Fields(htmlToPlainText(label)), " ")
					if label != "" {
						result = append(result, epubTOCEntry{Label: label, Href: href, Depth: maxInt(0, depth-1)})
					}
				}
			}
		case xml.EndElement:
			local := strings.ToLower(value.Name.Local)
			if inNav && (local == "ol" || local == "ul") && depth > 0 {
				depth--
			}
			if local == "nav" {
				inNav = false
			}
		}
	}
	return result
}

func parseEPUBNCX(data []byte) []epubTOCEntry {
	if len(data) == 0 {
		return nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	result := make([]epubTOCEntry, 0)
	depth := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch value := token.(type) {
		case xml.StartElement:
			if strings.EqualFold(value.Name.Local, "navPoint") {
				depth++
				var point struct {
					Label string `xml:"navLabel>text"`
					Src   string `xml:"content>src,attr"`
				}
				if decoder.DecodeElement(&point, &value) == nil && strings.TrimSpace(point.Label) != "" {
					result = append(result, epubTOCEntry{Label: strings.TrimSpace(point.Label), Href: strings.TrimSpace(point.Src), Depth: maxInt(0, depth-1)})
				}
				depth--
			}
		}
	}
	return result
}
