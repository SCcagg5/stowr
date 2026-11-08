package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// The editor of the lifecycle rules of a bucket (PUT /api/lifecycle). PutBucketLifecycleConfiguration replaces the
// whole configuration, so the server reads the configuration, keeps each element that the editor does not edit byte
// for byte, puts the edited rules next to them and writes the whole document. The page sends the SHA-256 hash of the
// document that it read: a different hash stops the write.
//
// The providers do not keep the same settings, and some drop a setting without an error (measured, see
// test/README.md). Garage drops a tag filter, and the rule then applies to every object under the prefix. Thus the
// editor offers a field only where the provider keeps it: a static table for each known provider, and for an unknown
// provider the expiration after N days with a prefix only, plus each field that a rule of the bucket already uses.
// After each write the server reads the configuration again and compares it with the document that it sent. It
// restores nothing: it tells the person what the provider changed, and does not offer again a field that the provider
// dropped. Only the S3 backend has the editor. The GCS backend answers 501.

const (
	// maxLifecycleRules is the limit of S3 for the rules of one configuration.
	maxLifecycleRules = 1000
	// maxLifecycleDays is the largest number of days of an action of the editor.
	maxLifecycleDays = 36500
	// maxLifecycleIDChars is the limit of S3 for the ID of a rule.
	maxLifecycleIDChars = 255
	// maxLifecyclePrefixBytes is the limit of S3 for a key, and so for a prefix.
	maxLifecyclePrefixBytes = 1024
	// maxLifecycleDocumentBytes is the largest configuration that the server reads.
	maxLifecycleDocumentBytes = 4 << 20

	lifecycleNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"
	// lifecycleFlavourProbeKey is the key of the GetObject request that tells the provider. The object does not exist.
	lifecycleFlavourProbeKey = ".stowr-provider-probe"
)

// The fields of the editor that a provider can drop or refuse. The expiration after N days with a prefix works on
// every provider that the tests know.
const (
	lifecycleFieldTag          = "tagFilter"
	lifecycleFieldNoncurrent   = "noncurrentExpiration"
	lifecycleFieldAbort        = "abortMultipart"
	lifecycleFieldDeleteMarker = "expiredDeleteMarker"
)

var lifecycleFields = []string{lifecycleFieldTag, lifecycleFieldNoncurrent, lifecycleFieldAbort, lifecycleFieldDeleteMarker}

var lifecycleFieldNames = map[string]string{
	lifecycleFieldTag:          "the tag filter",
	lifecycleFieldNoncurrent:   "the expiration of noncurrent versions",
	lifecycleFieldAbort:        "the abort of incomplete multipart uploads",
	lifecycleFieldDeleteMarker: "the removal of expired delete markers",
}

// The providers that Stowr can tell from their answers. An empty flavour is a provider that Stowr does not
// know.
const (
	flavourAWS       = "aws"
	flavourMinIO     = "minio"
	flavourSeaweedFS = "seaweedfs"
	flavourGarage    = "garage"
)

var flavourNames = map[string]string{flavourAWS: "AWS S3", flavourMinIO: "MinIO", flavourSeaweedFS: "SeaweedFS", flavourGarage: "Garage"}

// lifecycleProviderFields is the table of the fields that each known provider keeps. The facts come from requests to
// SeaweedFS 4.41, MinIO RELEASE.2025-10-15T17-29-55Z and Garage v2.4.1 (test/README.md), and from the documentation
// of AWS S3. A field that is not in the table of a provider is not offered for it.
var lifecycleProviderFields = map[string]map[string]bool{
	flavourAWS:       {lifecycleFieldTag: true, lifecycleFieldNoncurrent: true, lifecycleFieldAbort: true, lifecycleFieldDeleteMarker: true},
	flavourSeaweedFS: {lifecycleFieldTag: true, lifecycleFieldNoncurrent: true, lifecycleFieldAbort: true, lifecycleFieldDeleteMarker: true},
	flavourMinIO:     {lifecycleFieldTag: true, lifecycleFieldNoncurrent: true, lifecycleFieldDeleteMarker: true},
	flavourGarage:    {lifecycleFieldAbort: true},
}

// lifecycleProviderReasons tells why a known provider does not have a field.
var lifecycleProviderReasons = map[string]map[string]string{
	flavourMinIO: {
		lifecycleFieldAbort: "MinIO does not keep the abort of incomplete multipart uploads: it refuses a rule with this action only, and drops the action from a rule with another action.",
	},
	flavourGarage: {
		lifecycleFieldTag:          "Garage drops the tag filter of a rule without an error: the rule then applies to every object under the prefix.",
		lifecycleFieldNoncurrent:   "Garage keeps no versions and drops the expiration of noncurrent versions.",
		lifecycleFieldDeleteMarker: "Garage refuses the removal of expired delete markers.",
	},
}

// lifecycleProviderNotes are facts about the effect of the rules that the page shows for a provider.
var lifecycleProviderNotes = map[string]string{
	flavourSeaweedFS: "SeaweedFS keeps all these rules and applies them with its lifecycle worker, in a daily pass. Its TTL fast path at write is off by default and never applies to a bucket with versioning.",
	flavourGarage:    "Garage applies the expiration and the abort of incomplete uploads in a daily pass.",
}

// --- Backend --------------------------------------------------------------------------------------------------------

// lifecycleDocument is the configuration as the provider sent it. Body is nil when the bucket has no configuration.
type lifecycleDocument struct {
	Body []byte
}

// BucketLifecycleWriter is optional: a backend that reads the configuration of its bucket as XML, and writes or
// deletes it.
type BucketLifecycleWriter interface {
	ReadBucketLifecycle(ctx context.Context) (lifecycleDocument, error)
	WriteBucketLifecycle(ctx context.Context, body []byte) error
	DeleteBucketLifecycle(ctx context.Context) error
}

// BucketVersioningReader is optional: a backend that gives the versioning status of its bucket ("Enabled",
// "Suspended", or "" for a bucket that never had versioning).
type BucketVersioningReader interface {
	BucketVersioningStatus(ctx context.Context) (string, error)
}

// ProviderFlavourReader is optional: a backend that tells its provider from one read request.
type ProviderFlavourReader interface {
	ProviderFlavour(ctx context.Context) (string, error)
}

// ReadBucketLifecycle reads the configuration of the bucket as the provider sends it.
func (s *s3Backend) ReadBucketLifecycle(ctx context.Context) (lifecycleDocument, error) {
	query := url.Values{}
	query.Set("lifecycle", "")
	resp, err := s.do(ctx, http.MethodGet, "", query, nil, 0, nil)
	if err != nil {
		return lifecycleDocument{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		var failure struct {
			Code string `xml:"Code"`
		}
		_ = xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&failure)
		if failure.Code == "NoSuchLifecycleConfiguration" {
			return lifecycleDocument{}, nil
		}
		return lifecycleDocument{}, &upstreamError{StatusCode: http.StatusNotFound, Code: failure.Code}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return lifecycleDocument{}, s3ResponseError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLifecycleDocumentBytes+1))
	if err != nil {
		return lifecycleDocument{}, fmt.Errorf("read s3 lifecycle response: %w", err)
	}
	if len(body) > maxLifecycleDocumentBytes {
		return lifecycleDocument{}, fmt.Errorf("the s3 lifecycle configuration is larger than %d bytes", maxLifecycleDocumentBytes)
	}
	return lifecycleDocument{Body: body}, nil
}

// WriteBucketLifecycle replaces the configuration of the bucket (PutBucketLifecycleConfiguration).
func (s *s3Backend) WriteBucketLifecycle(ctx context.Context, body []byte) error {
	query := url.Values{}
	query.Set("lifecycle", "")
	sum := md5.Sum(body)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/xml")
	headers.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	resp, err := s.do(ctx, http.MethodPut, "", query, bytes.NewReader(body), int64(len(body)), headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s3ResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// DeleteBucketLifecycle removes the configuration. MinIO and Garage refuse a configuration without a rule.
func (s *s3Backend) DeleteBucketLifecycle(ctx context.Context) error {
	query := url.Values{}
	query.Set("lifecycle", "")
	resp, err := s.do(ctx, http.MethodDelete, "", query, nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s3ResponseError(resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// BucketVersioningStatus reads GetBucketVersioning. SeaweedFS, MinIO and Garage send no status for a bucket that never
// had versioning.
func (s *s3Backend) BucketVersioningStatus(ctx context.Context) (string, error) {
	query := url.Values{}
	query.Set("versioning", "")
	resp, err := s.do(ctx, http.MethodGet, "", query, nil, 0, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", s3ResponseError(resp)
	}
	var result struct {
		Status string `xml:"Status"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("decode s3 versioning response: %w", err)
	}
	return strings.TrimSpace(result.Status), nil
}

// ProviderFlavour reads an object that does not exist and tells the provider from the answer.
func (s *s3Backend) ProviderFlavour(ctx context.Context) (string, error) {
	resp, err := s.do(ctx, http.MethodGet, s.cfg.RootPrefix+lifecycleFlavourProbeKey, nil, nil, 0, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return s3ProviderFlavour(s.endpoint, resp.Header, body), nil
}

// s3ProviderFlavour tells the provider from the endpoint, the Server header and the error document of an answer.
// AWS S3 sends "AmazonS3", MinIO "MinIO" and SeaweedFS "SeaweedFS <size> <version>". Garage v2.4.1 sends no Server
// header and no request id, and its error document has a Region element and no RequestId element. Any other answer
// gives "": Stowr does not know the provider.
func s3ProviderFlavour(endpoint *url.URL, header http.Header, body []byte) string {
	host := ""
	if endpoint != nil {
		host = strings.ToLower(endpoint.Hostname())
	}
	server := strings.ToLower(strings.TrimSpace(header.Get("Server")))
	switch {
	case host == "amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com") || server == "amazons3":
		return flavourAWS
	case strings.HasPrefix(server, "minio"):
		return flavourMinIO
	case strings.HasPrefix(server, "seaweedfs"):
		return flavourSeaweedFS
	case strings.HasPrefix(server, "garage"):
		return flavourGarage
	case server == "" && header.Get("x-amz-request-id") == "" && header.Get("x-amz-id-2") == "" && garageErrorDocument(body):
		return flavourGarage
	}
	return ""
}

func garageErrorDocument(body []byte) bool {
	var document struct {
		XMLName   xml.Name
		Code      string  `xml:"Code"`
		Region    *string `xml:"Region"`
		RequestID *string `xml:"RequestId"`
	}
	if xml.Unmarshal(body, &document) != nil {
		return false
	}
	return document.XMLName.Local == "Error" && document.Code != "" && document.Region != nil && document.RequestID == nil
}

// --- XML of a configuration -----------------------------------------------------------------------------------------

// xmlNode is an element of a configuration, without its namespace. A leaf has its text, trimmed.
type xmlNode struct {
	Name     string
	Attrs    []xml.Attr
	Text     string
	Children []*xmlNode
	// Padded is true for a leaf whose text has spaces at the start or at the end. The editor keeps such a rule as is:
	// a prefix "logs " is not the prefix "logs".
	Padded bool
}

func (n *xmlNode) child(name string) *xmlNode {
	for _, child := range n.Children {
		if child.Name == name {
			return child
		}
	}
	return nil
}

// canonical gives a text that is the same for two elements with the same names, attributes, texts and children. The
// order of the children does not count: MinIO sends the children of And in another order.
func (n *xmlNode) canonical() string {
	var builder strings.Builder
	builder.WriteString("<" + n.Name)
	attrs := make([]string, 0, len(n.Attrs))
	for _, attr := range n.Attrs {
		attrs = append(attrs, attr.Name.Local+"="+strconv.Quote(attr.Value))
	}
	sort.Strings(attrs)
	for _, attr := range attrs {
		builder.WriteString(" " + attr)
	}
	builder.WriteString(">")
	children := make([]string, 0, len(n.Children))
	for _, child := range n.Children {
		children = append(children, child.canonical())
	}
	sort.Strings(children)
	builder.WriteString(strconv.Quote(n.Text))
	builder.WriteString(strings.Join(children, ""))
	builder.WriteString("</" + n.Name + ">")
	return builder.String()
}

// readXMLNode reads the element that start opens, up to its end.
func readXMLNode(decoder *xml.Decoder, start xml.StartElement) (*xmlNode, error) {
	node := &xmlNode{Name: start.Name.Local}
	for _, attr := range start.Attr {
		if attr.Name.Space == "xmlns" || (attr.Name.Space == "" && attr.Name.Local == "xmlns") {
			continue
		}
		node.Attrs = append(node.Attrs, attr)
	}
	var text strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			child, err := readXMLNode(decoder, value)
			if err != nil {
				return nil, err
			}
			node.Children = append(node.Children, child)
		case xml.CharData:
			text.Write(value)
		case xml.EndElement:
			node.Text = strings.TrimSpace(text.String())
			node.Padded = len(node.Children) == 0 && node.Text != text.String()
			return node, nil
		}
	}
}

// lifecycleItem is one element of the root of a configuration, with its bytes as the provider sent them.
type lifecycleItem struct {
	Raw  []byte
	Node *xmlNode
}

// lifecycleConfig is a configuration split into its elements. RootStart is the start tag of the root as the provider
// sent it; it is empty for a bucket without a configuration.
type lifecycleConfig struct {
	RootStart []byte
	RootName  string
	Items     []lifecycleItem
}

var xmlStartName = regexp.MustCompile(`^<([^\s/>]+)`)

// splitLifecycleDocument cuts the document into the elements of its root. The bytes of each element are the bytes of
// the document: a rule that the editor keeps goes back to the provider unchanged.
func splitLifecycleDocument(body []byte) (lifecycleConfig, error) {
	var config lifecycleConfig
	if len(bytes.TrimSpace(body)) == 0 {
		return config, nil
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		before := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return config, fmt.Errorf("the lifecycle configuration is not valid XML: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "LifecycleConfiguration" {
			return config, fmt.Errorf("the lifecycle configuration starts with the element %s", start.Name.Local)
		}
		config.RootStart = append([]byte(nil), body[before:decoder.InputOffset()]...)
		if match := xmlStartName.FindSubmatch(config.RootStart); match != nil {
			config.RootName = string(match[1])
		}
		if bytes.HasSuffix(bytes.TrimSpace(config.RootStart), []byte("/>")) {
			// <LifecycleConfiguration/>: a root without an element. The writer opens it again.
			config.RootStart = config.RootStart[:0]
			return config, nil
		}
		break
	}
	for {
		before := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return config, fmt.Errorf("the lifecycle configuration is not valid XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			node, err := readXMLNode(decoder, value)
			if err != nil {
				return config, fmt.Errorf("the lifecycle configuration is not valid XML: %w", err)
			}
			config.Items = append(config.Items, lifecycleItem{Raw: append([]byte(nil), body[before:decoder.InputOffset()]...), Node: node})
		case xml.EndElement:
			return config, nil
		}
	}
}

// lifecycleHash is the hash that the page sends back: the SHA-256 of the document as the provider sent it, and of the
// indexes of the rules that the editor showed as editable. A rule that becomes "kept as is" (a field that the provider
// dropped in another save) also stops the save: the page shows other rules than the server would write.
func lifecycleHash(body []byte, editable []int) string {
	hash := sha256.New()
	hash.Write(body)
	for _, index := range editable {
		fmt.Fprintf(hash, "\x00%d", index)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// --- Rules of the editor --------------------------------------------------------------------------------------------

// lifecycleEditRule is a rule that the editor shows and writes. Prefix is relative to the root prefix of the instance.
// A zero number of days means "no such action".
type lifecycleEditRule struct {
	ID                  string     `json:"id"`
	Enabled             bool       `json:"enabled"`
	Prefix              string     `json:"prefix"`
	Tag                 *objectTag `json:"tag,omitempty"`
	ExpirationDays      int        `json:"expirationDays,omitempty"`
	NoncurrentDays      int        `json:"noncurrentDays,omitempty"`
	AbortMultipartDays  int        `json:"abortMultipartDays,omitempty"`
	ExpiredDeleteMarker bool       `json:"expiredDeleteMarker,omitempty"`
}

// fields gives the fields of the provider table that the rule uses.
func (rule lifecycleEditRule) fields() []string {
	var used []string
	if rule.Tag != nil {
		used = append(used, lifecycleFieldTag)
	}
	if rule.NoncurrentDays > 0 {
		used = append(used, lifecycleFieldNoncurrent)
	}
	if rule.AbortMultipartDays > 0 {
		used = append(used, lifecycleFieldAbort)
	}
	if rule.ExpiredDeleteMarker {
		used = append(used, lifecycleFieldDeleteMarker)
	}
	return used
}

func invalidLifecycle(format string, values ...any) error {
	return apiError{Status: http.StatusBadRequest, Code: "invalid_lifecycle", Message: fmt.Sprintf(format, values...)}
}

func lifecycleUnsupported() error {
	return apiError{Status: http.StatusNotImplemented, Code: "lifecycle_unsupported", Message: "this storage provider does not give lifecycle rules"}
}

func lifecycleChangedError() error {
	return apiError{Status: http.StatusConflict, Code: "lifecycle_changed", Message: "the lifecycle rules of the bucket changed after the window read them: read them again and make the change again"}
}

// ruleName is the name of a rule in a message.
func ruleName(id string) string {
	if id == "" {
		return "without an ID"
	}
	return strconv.Quote(id)
}

// validateLifecycleEditRule checks one rule of the editor. The checks follow the limits of S3: AWS refuses the abort
// of incomplete uploads and the removal of delete markers in a rule with a tag filter, and the removal of delete
// markers next to an expiration in days.
func validateLifecycleEditRule(rule lifecycleEditRule) error {
	name := ruleName(rule.ID)
	switch {
	case strings.TrimSpace(rule.ID) == "":
		return invalidLifecycle("each rule needs an ID")
	case rule.ID != strings.TrimSpace(rule.ID):
		return invalidLifecycle("the ID of the rule %s starts or ends with a space", name)
	case utf8.RuneCountInString(rule.ID) > maxLifecycleIDChars:
		return invalidLifecycle("the ID of a rule has at most %d characters, got %d", maxLifecycleIDChars, utf8.RuneCountInString(rule.ID))
	case !utf8.ValidString(rule.ID) || strings.ContainsFunc(rule.ID, isControlRune):
		return invalidLifecycle("the ID of the rule %s has a control character", name)
	case len(rule.Prefix) > maxLifecyclePrefixBytes:
		return invalidLifecycle("the prefix of the rule %s has more than %d bytes", name, maxLifecyclePrefixBytes)
	case !utf8.ValidString(rule.Prefix) || strings.ContainsFunc(rule.Prefix, isControlRune):
		return invalidLifecycle("the prefix of the rule %s has a control character", name)
	}
	for _, days := range []struct {
		value int
		what  string
	}{{rule.ExpirationDays, "the expiration"}, {rule.NoncurrentDays, "the expiration of noncurrent versions"}, {rule.AbortMultipartDays, "the abort of incomplete uploads"}} {
		if days.value < 0 || days.value > maxLifecycleDays {
			return invalidLifecycle("the number of days of %s of the rule %s is a whole number from 1 to %d, got %d", days.what, name, maxLifecycleDays, days.value)
		}
	}
	if rule.ExpirationDays == 0 && rule.NoncurrentDays == 0 && rule.AbortMultipartDays == 0 && !rule.ExpiredDeleteMarker {
		return invalidLifecycle("the rule %s has no action: give at least one", name)
	}
	if rule.ExpirationDays > 0 && rule.ExpiredDeleteMarker {
		return invalidLifecycle("the rule %s cannot expire the objects after a number of days and remove the expired delete markers: use two rules", name)
	}
	if rule.Tag != nil {
		if err := validateTags([]objectTag{*rule.Tag}); err != nil {
			return invalidLifecycle("the tag filter of the rule %s: %s", name, err.(apiError).Message)
		}
		if rule.AbortMultipartDays > 0 || rule.ExpiredDeleteMarker {
			return invalidLifecycle("the rule %s has a tag filter: S3 does not let such a rule abort incomplete uploads or remove delete markers", name)
		}
	}
	return nil
}

func isControlRune(r rune) bool {
	return r < 0x20 || r == 0x7f
}

// lifecycleRuleXML gives the XML of a rule of the editor. prefix is the full prefix in the bucket.
func lifecycleRuleXML(rule lifecycleEditRule, prefix string) []byte {
	var buffer bytes.Buffer
	text := func(value string) {
		_ = xml.EscapeText(&buffer, []byte(value))
	}
	buffer.WriteString("<Rule><ID>")
	text(rule.ID)
	buffer.WriteString("</ID><Filter>")
	switch {
	case rule.Tag != nil && prefix != "":
		buffer.WriteString("<And><Prefix>")
		text(prefix)
		buffer.WriteString("</Prefix><Tag><Key>")
		text(rule.Tag.Key)
		buffer.WriteString("</Key><Value>")
		text(rule.Tag.Value)
		buffer.WriteString("</Value></Tag></And>")
	case rule.Tag != nil:
		buffer.WriteString("<Tag><Key>")
		text(rule.Tag.Key)
		buffer.WriteString("</Key><Value>")
		text(rule.Tag.Value)
		buffer.WriteString("</Value></Tag>")
	default:
		buffer.WriteString("<Prefix>")
		text(prefix)
		buffer.WriteString("</Prefix>")
	}
	buffer.WriteString("</Filter><Status>")
	if rule.Enabled {
		buffer.WriteString("Enabled")
	} else {
		buffer.WriteString("Disabled")
	}
	buffer.WriteString("</Status>")
	if rule.ExpirationDays > 0 {
		fmt.Fprintf(&buffer, "<Expiration><Days>%d</Days></Expiration>", rule.ExpirationDays)
	} else if rule.ExpiredDeleteMarker {
		buffer.WriteString("<Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration>")
	}
	if rule.NoncurrentDays > 0 {
		fmt.Fprintf(&buffer, "<NoncurrentVersionExpiration><NoncurrentDays>%d</NoncurrentDays></NoncurrentVersionExpiration>", rule.NoncurrentDays)
	}
	if rule.AbortMultipartDays > 0 {
		fmt.Fprintf(&buffer, "<AbortIncompleteMultipartUpload><DaysAfterInitiation>%d</DaysAfterInitiation></AbortIncompleteMultipartUpload>", rule.AbortMultipartDays)
	}
	buffer.WriteString("</Rule>")
	return buffer.Bytes()
}

// leafNode reports an element with a text, no child, no attribute, and no space around the text.
func leafNode(node *xmlNode) bool {
	return node != nil && len(node.Children) == 0 && len(node.Attrs) == 0 && !node.Padded
}

// onlyChildren reports an element without attributes and text whose children have the allowed names, one of each.
func onlyChildren(node *xmlNode, allowed ...string) bool {
	if len(node.Attrs) > 0 || node.Text != "" {
		return false
	}
	seen := map[string]bool{}
	for _, child := range node.Children {
		if seen[child.Name] || !containsString(allowed, child.Name) {
			return false
		}
		seen[child.Name] = true
	}
	return true
}

func lifecycleDaysValue(node *xmlNode) (int, bool) {
	if !leafNode(node) {
		return 0, false
	}
	days, err := strconv.Atoi(node.Text)
	return days, err == nil && days >= 1 && days <= maxLifecycleDays
}

// tagFromNode gives the tag of a filter. A key with "=" or ",", or a value with ",", stays out of the editor: the
// page writes a tag as key=value, and would read it back as another tag.
func tagFromNode(node *xmlNode) (*objectTag, bool) {
	if !onlyChildren(node, "Key", "Value") || !leafNode(node.child("Key")) || strings.ContainsAny(node.child("Key").Text, "=,") {
		return nil, false
	}
	tag := objectTag{Key: node.child("Key").Text}
	if value := node.child("Value"); value != nil {
		if !leafNode(value) || strings.Contains(value.Text, ",") {
			return nil, false
		}
		tag.Value = value.Text
	}
	return &tag, true
}

// lifecycleKeptReason gives why the editor keeps a rule as is, from the elements that it does not edit.
func lifecycleKeptReason(node *xmlNode) string {
	has := func(path ...string) bool {
		current := node
		for _, name := range path {
			if current = current.child(name); current == nil {
				return false
			}
		}
		return true
	}
	switch {
	case has("Transition") || has("NoncurrentVersionTransition"):
		return "It moves objects to another storage class."
	case has("Expiration", "Date"):
		return "It expires the objects on a date."
	case has("Filter", "ObjectSizeGreaterThan") || has("Filter", "ObjectSizeLessThan") || has("Filter", "And", "ObjectSizeGreaterThan") || has("Filter", "And", "ObjectSizeLessThan"):
		return "It filters the objects by size."
	case has("NoncurrentVersionExpiration", "NewerNoncurrentVersions"):
		return "It keeps a number of noncurrent versions."
	case has("Prefix"):
		return "It has the old Prefix element outside of a filter."
	case node.child("ID") == nil || node.child("ID").Text == "":
		return "It has no ID."
	}
	if and := node.child("Filter"); and != nil && and.child("And") != nil {
		count := 0
		for _, child := range and.child("And").Children {
			if child.Name == "Tag" {
				count++
			}
		}
		if count > 1 {
			return "It filters by more than one tag."
		}
	}
	return "It has settings that the editor does not show."
}

// lifecycleEditRuleFromNode gives the rule of the editor for an element, or a reason to keep it as is. rootPrefix is
// the root prefix of the instance: a rule outside of it is kept as is.
func lifecycleEditRuleFromNode(node *xmlNode, rootPrefix string) (lifecycleEditRule, string) {
	var rule lifecycleEditRule
	if node.Name != "Rule" || !onlyChildren(node, "ID", "Status", "Filter", "Expiration", "NoncurrentVersionExpiration", "AbortIncompleteMultipartUpload") {
		return rule, lifecycleKeptReason(node)
	}
	id, status := node.child("ID"), node.child("Status")
	if !leafNode(id) || id.Text == "" || !leafNode(status) || (status.Text != "Enabled" && status.Text != "Disabled") {
		return rule, lifecycleKeptReason(node)
	}
	rule.ID, rule.Enabled = id.Text, status.Text == "Enabled"
	prefix := ""
	if filter := node.child("Filter"); filter != nil {
		if !onlyChildren(filter, "Prefix", "Tag", "And") || len(filter.Children) > 1 {
			return rule, lifecycleKeptReason(node)
		}
		for _, child := range filter.Children {
			switch child.Name {
			case "Prefix":
				if !leafNode(child) {
					return rule, lifecycleKeptReason(node)
				}
				prefix = child.Text
			case "Tag":
				tag, ok := tagFromNode(child)
				if !ok {
					return rule, lifecycleKeptReason(node)
				}
				rule.Tag = tag
			case "And":
				if !onlyChildren(child, "Prefix", "Tag") || child.child("Tag") == nil {
					return rule, lifecycleKeptReason(node)
				}
				if andPrefix := child.child("Prefix"); andPrefix != nil {
					if !leafNode(andPrefix) {
						return rule, lifecycleKeptReason(node)
					}
					prefix = andPrefix.Text
				}
				tag, ok := tagFromNode(child.child("Tag"))
				if !ok {
					return rule, lifecycleKeptReason(node)
				}
				rule.Tag = tag
			}
		}
	}
	if expiration := node.child("Expiration"); expiration != nil {
		if !onlyChildren(expiration, "Days", "ExpiredObjectDeleteMarker") || len(expiration.Children) != 1 {
			return rule, lifecycleKeptReason(node)
		}
		if days := expiration.child("Days"); days != nil {
			value, ok := lifecycleDaysValue(days)
			if !ok {
				return rule, lifecycleKeptReason(node)
			}
			rule.ExpirationDays = value
		} else if marker := expiration.child("ExpiredObjectDeleteMarker"); !leafNode(marker) || marker.Text != "true" {
			return rule, lifecycleKeptReason(node)
		} else {
			rule.ExpiredDeleteMarker = true
		}
	}
	if noncurrent := node.child("NoncurrentVersionExpiration"); noncurrent != nil {
		value, ok := 0, onlyChildren(noncurrent, "NoncurrentDays") && len(noncurrent.Children) == 1
		if ok {
			value, ok = lifecycleDaysValue(noncurrent.child("NoncurrentDays"))
		}
		if !ok {
			return rule, lifecycleKeptReason(node)
		}
		rule.NoncurrentDays = value
	}
	if abort := node.child("AbortIncompleteMultipartUpload"); abort != nil {
		value, ok := 0, onlyChildren(abort, "DaysAfterInitiation") && len(abort.Children) == 1
		if ok {
			value, ok = lifecycleDaysValue(abort.child("DaysAfterInitiation"))
		}
		if !ok {
			return rule, lifecycleKeptReason(node)
		}
		rule.AbortMultipartDays = value
	}
	if !strings.HasPrefix(prefix, rootPrefix) {
		return rule, lifecycleReasonOutside
	}
	rule.Prefix = strings.TrimPrefix(prefix, rootPrefix)
	if err := validateLifecycleEditRule(rule); err != nil {
		return rule, lifecycleReasonValues
	}
	return rule, ""
}

// The reasons of lifecycleEditRuleFromNode that the code compares. With lifecycleReasonValues, the rule has all its
// fields: only a check of validateLifecycleEditRule failed. With any other reason, the fields are not complete.
const (
	lifecycleReasonOutside = "It applies outside of the folder of this instance."
	lifecycleReasonValues  = "Its values are outside of the limits of the editor."
)

// sameEditRule reports two rules of the editor with the same fields.
func sameEditRule(left, right lifecycleEditRule) bool {
	if (left.Tag == nil) != (right.Tag == nil) || (left.Tag != nil && *left.Tag != *right.Tag) {
		return false
	}
	left.Tag, right.Tag = nil, nil
	return left == right
}

// destructive reports an enabled rule that deletes objects, versions or delete markers.
func (rule lifecycleEditRule) destructive() bool {
	return rule.Enabled && (rule.ExpirationDays > 0 || rule.NoncurrentDays > 0 || rule.ExpiredDeleteMarker)
}

// --- Capability of an instance --------------------------------------------------------------------------------------

// lifecycleCapability is the state of the lifecycle editor of an instance in this process.
type lifecycleCapability struct {
	// off is set when the provider refused the read of the configuration (501 or 405).
	off atomic.Bool
	// write makes the read, the hash check, the write and the read-back of one save one step in this process.
	write sync.Mutex

	mu           sync.Mutex
	flavour      string
	flavourKnown bool
	// writeRefusal is the reason of the last refusal of a write (403, 501 or 405). A later write that works clears it.
	writeRefusal string
	// dropped has the fields that the provider dropped after a save in this process.
	dropped map[string]bool
	// ruleCount is the number of rules at the last read of the configuration. ruleCountKnown is set after the first
	// read that worked. The bucket selector shows the count for each bucket.
	ruleCount      int
	ruleCountKnown bool
}

// lifecycleAvailable tells if the instance gives the lifecycle rules. Until the probe has run, the answer is yes for a
// backend with the rules.
func (s *storageInstance) lifecycleAvailable() bool {
	if s == nil {
		return false
	}
	if _, ok := s.backend.(BucketLifecycleReader); !ok {
		return false
	}
	return !s.lifecycle.off.Load()
}

// probeLifecycle reads the configuration of the bucket, and the answer to an object that does not exist. A provider
// without the lifecycle answers 501 or 405. The second answer tells the provider. Both requests are reads. Any other
// failure leaves the answer at yes. An instance without the read permission is not probed.
func (s *storageInstance) probeLifecycle(ctx context.Context) {
	if _, ok := s.backend.(BucketLifecycleReader); !ok || !s.permitted(permissionRead) {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var err error
	var rules []lifecycleRule
	if writer, ok := s.backend.(BucketLifecycleWriter); ok {
		var document lifecycleDocument
		if document, err = writer.ReadBucketLifecycle(probeCtx); err == nil {
			rules, err = parseLifecycleRules(document.Body)
		}
	} else {
		rules, err = s.backend.(BucketLifecycleReader).GetBucketLifecycle(probeCtx)
	}
	if providerRefusedFeature(err) {
		s.lifecycle.off.Store(true)
		return
	}
	if err == nil {
		s.setLifecycleRuleCount(len(rules))
	}
	s.lifecycleFlavour(probeCtx)
}

// setLifecycleRuleCount keeps the number of rules of the last read of the configuration.
func (s *storageInstance) setLifecycleRuleCount(count int) {
	s.lifecycle.mu.Lock()
	defer s.lifecycle.mu.Unlock()
	s.lifecycle.ruleCount, s.lifecycle.ruleCountKnown = count, true
}

// lifecycleRuleCount gives the number of rules for /api/instances, or nil when the server has not read the rules or the
// instance has no lifecycle rules.
func (s *storageInstance) lifecycleRuleCount() *int {
	if !s.lifecycleAvailable() {
		return nil
	}
	s.lifecycle.mu.Lock()
	defer s.lifecycle.mu.Unlock()
	if !s.lifecycle.ruleCountKnown {
		return nil
	}
	count := s.lifecycle.ruleCount
	return &count
}

// bucketFactsTTL is the age of the versioning status and of the number of lifecycle rules after which a request of
// /api/instances reads them again. The read is in the background: the request does not wait for it.
const bucketFactsTTL = 60 * time.Second

// bucketFactsState is the time of the last read of the facts of a bucket, and the read in flight.
type bucketFactsState struct {
	mu      sync.Mutex
	at      time.Time
	running bool
}

func (s *storageInstance) markBucketFactsRead() {
	s.facts.mu.Lock()
	defer s.facts.mu.Unlock()
	s.facts.at = time.Now()
}

// refreshBucketFacts reads the versioning status and the lifecycle rules of the bucket again when the last read is
// older than bucketFactsTTL. One read at a time for each instance, 5 seconds at most, inside the limits of the storage
// requests. A failed read keeps the last values. Before the probe at start, nothing is read.
func (s *storageInstance) refreshBucketFacts() {
	if s == nil || !s.permitted(permissionRead) {
		return
	}
	s.facts.mu.Lock()
	if s.facts.running || s.facts.at.IsZero() || time.Since(s.facts.at) < bucketFactsTTL {
		s.facts.mu.Unlock()
		return
	}
	s.facts.running = true
	s.facts.mu.Unlock()
	go func() {
		defer func() {
			s.facts.mu.Lock()
			s.facts.running, s.facts.at = false, time.Now()
			s.facts.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.bucketVersioning(ctx)
		writer, err := s.lifecycleWriter()
		if err != nil {
			return
		}
		release, err := s.beginStorageRequest(ctx)
		if err != nil {
			return
		}
		document, err := writer.ReadBucketLifecycle(ctx)
		release()
		if err != nil {
			return
		}
		if rules, err := parseLifecycleRules(document.Body); err == nil {
			s.setLifecycleRuleCount(len(rules))
		}
	}()
}

// knownFlavour gives the provider that an earlier probe recognised, or "": it never asks the provider.
func (s *storageInstance) knownFlavour() string {
	if s == nil {
		return ""
	}
	s.lifecycle.mu.Lock()
	defer s.lifecycle.mu.Unlock()
	if !s.lifecycle.flavourKnown {
		return ""
	}
	return s.lifecycle.flavour
}

// lifecycleFlavour gives the provider of the instance, and asks it one time.
func (s *storageInstance) lifecycleFlavour(ctx context.Context) string {
	s.lifecycle.mu.Lock()
	if s.lifecycle.flavourKnown {
		defer s.lifecycle.mu.Unlock()
		return s.lifecycle.flavour
	}
	s.lifecycle.mu.Unlock()
	reader, ok := s.backend.(ProviderFlavourReader)
	if !ok {
		return ""
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	flavour, err := reader.ProviderFlavour(probeCtx)
	if err != nil {
		return ""
	}
	s.lifecycle.mu.Lock()
	defer s.lifecycle.mu.Unlock()
	s.lifecycle.flavour, s.lifecycle.flavourKnown = flavour, true
	return flavour
}

func (s *storageInstance) lifecycleWriteRefusal() string {
	s.lifecycle.mu.Lock()
	defer s.lifecycle.mu.Unlock()
	return s.lifecycle.writeRefusal
}

func (s *storageInstance) setLifecycleWriteRefusal(reason string) {
	s.lifecycle.mu.Lock()
	defer s.lifecycle.mu.Unlock()
	s.lifecycle.writeRefusal = reason
}

func (s *storageInstance) lifecycleDropped(field string) bool {
	s.lifecycle.mu.Lock()
	defer s.lifecycle.mu.Unlock()
	return s.lifecycle.dropped[field]
}

func (s *storageInstance) markLifecycleDropped(field string) {
	s.lifecycle.mu.Lock()
	defer s.lifecycle.mu.Unlock()
	if s.lifecycle.dropped == nil {
		s.lifecycle.dropped = map[string]bool{}
	}
	s.lifecycle.dropped[field] = true
}

// lifecycleHiddenField is a field that the editor does not offer, with the reason for the person.
type lifecycleHiddenField struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// lifecycleFieldsFor decides the fields of the editor. A field needs the provider (the table of a known provider, or
// for an unknown provider a rule of the bucket that uses it), and its condition: the tags of the instance for the tag
// filter, and the versioning of the bucket for the noncurrent versions and the delete markers. A field that the
// provider dropped after a save in this process is never offered again. The reason of the provider table comes before
// the reason of a condition: it tells what the provider does with the field.
func (s *storageInstance) lifecycleFieldsFor(flavour, versioning string, used map[string]bool) (map[string]bool, []lifecycleHiddenField) {
	fields := make(map[string]bool, len(lifecycleFields))
	hidden := []lifecycleHiddenField{}
	versioned := versioning == "Enabled" || versioning == "Suspended"
	for _, field := range lifecycleFields {
		reason := ""
		switch {
		case s.lifecycleDropped(field):
			reason = "The storage provider dropped this setting after a save: Stowr does not offer it again."
		case flavour != "" && !lifecycleProviderFields[flavour][field]:
			reason = lifecycleProviderReasons[flavour][field]
			if reason == "" {
				reason = fmt.Sprintf("%s does not keep this setting.", flavourNames[flavour])
			}
		case field == lifecycleFieldTag && !s.tagsAvailable():
			reason = "This storage provider keeps no object tags."
		case (field == lifecycleFieldNoncurrent || field == lifecycleFieldDeleteMarker) && !versioned:
			reason = "The bucket has no versioning."
		case flavour == "" && !used[field]:
			reason = "Stowr does not know this storage provider, and no rule of the bucket uses this setting: the provider can drop it without an error."
		}
		fields[field] = reason == ""
		if reason != "" {
			hidden = append(hidden, lifecycleHiddenField{Field: field, Reason: reason})
		}
	}
	return fields, hidden
}

// --- State of the editor --------------------------------------------------------------------------------------------

// lifecycleEntry is one rule of the configuration. An editable rule has Rule. A rule that the editor keeps as is has
// its XML and the reason. Entries and the rules of lifecycleResponse have the same order.
type lifecycleEntry struct {
	Editable bool               `json:"editable"`
	Rule     *lifecycleEditRule `json:"rule,omitempty"`
	XML      string             `json:"xml,omitempty"`
	Reason   string             `json:"reason,omitempty"`
}

type lifecycleEditorState struct {
	// Hash is the SHA-256 of the configuration that the server read. The page sends it back with the change.
	Hash    string           `json:"hash"`
	Entries []lifecycleEntry `json:"entries"`
	// Fields tells which fields the editor offers. Hidden gives the reason for each other field.
	Fields map[string]bool        `json:"fields"`
	Hidden []lifecycleHiddenField `json:"hidden"`
	// Provider is the provider that Stowr found ("aws", "minio", "seaweedfs", "garage"), or "".
	Provider     string `json:"provider"`
	ProviderNote string `json:"providerNote,omitempty"`
	Versioning   string `json:"versioning"`
	RootPrefix   string `json:"rootPrefix"`
	// Writable is false when the person or the provider cannot change the rules. ReadOnlyReason tells why.
	Writable       bool   `json:"writable"`
	ReadOnlyReason string `json:"readOnlyReason,omitempty"`
	MaxDays        int    `json:"maxDays"`
	MaxRules       int    `json:"maxRules"`
}

// lifecycleState is a configuration read for the editor.
type lifecycleState struct {
	body   []byte
	config lifecycleConfig
	rules  []lifecycleRule
	editor lifecycleEditorState
	// edit has the rule of the editor for each editable entry, at the index of the entry. raw has the bytes of each
	// editable rule, by ID: a rule that the save does not change goes back with its bytes.
	edit map[int]lifecycleEditRule
	raw  map[string][]byte
	// keptIDs are the IDs of the rules that the editor keeps as is.
	keptIDs []string
}

func (s *storageInstance) lifecycleWriter() (BucketLifecycleWriter, error) {
	writer, ok := s.backend.(BucketLifecycleWriter)
	if !ok || s.lifecycle.off.Load() {
		return nil, lifecycleUnsupported()
	}
	return writer, nil
}

func (s *storageInstance) bucketVersioning(ctx context.Context) string {
	reader, ok := s.backend.(BucketVersioningReader)
	if !ok || !s.versioningAvailable() {
		return ""
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return ""
	}
	defer release()
	status, err := reader.BucketVersioningStatus(ctx)
	if err != nil {
		return ""
	}
	s.setVersioningStatus(status)
	return status
}

// readLifecycleState reads the configuration and decides the rules that the editor shows.
func (s *storageInstance) readLifecycleState(ctx context.Context) (lifecycleState, error) {
	writer, err := s.lifecycleWriter()
	if err != nil {
		return lifecycleState{}, err
	}
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return lifecycleState{}, err
	}
	document, err := writer.ReadBucketLifecycle(ctx)
	release()
	if providerRefusedFeature(err) {
		s.lifecycle.off.Store(true)
		return lifecycleState{}, lifecycleUnsupported()
	}
	s.observePermission(permissionRead, err)
	if err != nil {
		return lifecycleState{}, err
	}
	state := lifecycleState{body: document.Body, edit: map[int]lifecycleEditRule{}, raw: map[string][]byte{}}
	if state.config, err = splitLifecycleDocument(document.Body); err != nil {
		return lifecycleState{}, apiError{Status: http.StatusBadGateway, Code: "lifecycle_invalid", Message: err.Error()}
	}
	if state.rules, err = parseLifecycleRules(document.Body); err != nil {
		return lifecycleState{}, apiError{Status: http.StatusBadGateway, Code: "lifecycle_invalid", Message: err.Error()}
	}
	s.setLifecycleRuleCount(len(state.rules))
	flavour := s.lifecycleFlavour(ctx)
	versioning := s.bucketVersioning(ctx)
	rootPrefix := s.cfg.RootPrefix
	// The shape of each rule first: for an unknown provider, a field that a rule of the bucket already uses is a field
	// that the provider keeps.
	used := map[string]bool{}
	for _, item := range state.config.Items {
		if item.Node.Name != "Rule" {
			continue
		}
		if rule, reason := lifecycleEditRuleFromNode(item.Node, ""); reason == "" {
			for _, field := range rule.fields() {
				used[field] = true
			}
		}
	}
	fields, hidden := s.lifecycleFieldsFor(flavour, versioning, used)
	entries := make([]lifecycleEntry, 0, len(state.config.Items))
	editable := []int{}
	for _, item := range state.config.Items {
		if item.Node.Name != "Rule" {
			continue
		}
		rule, reason := lifecycleEditRuleFromNode(item.Node, rootPrefix)
		if reason == "" {
			for _, field := range rule.fields() {
				if !fields[field] {
					reason = fmt.Sprintf("It uses %s, which the editor does not offer here.", lifecycleFieldNames[field])
					break
				}
			}
		}
		entry := lifecycleEntry{Editable: reason == ""}
		switch {
		case entry.Editable:
			edited := rule
			entry.Rule = &edited
			state.edit[len(entries)] = rule
			state.raw[rule.ID] = item.Raw
			editable = append(editable, len(entries))
		case reason == lifecycleReasonOutside:
			// A rule of another part of the bucket: the person of this instance does not see its XML or its filter.
			entry.Reason = reason
			state.keptIDs = append(state.keptIDs, state.rules[len(entries)].ID)
			state.rules[len(entries)] = lifecycleRule{Enabled: state.rules[len(entries)].Enabled}
		default:
			entry.XML, entry.Reason = string(item.Raw), reason
			state.keptIDs = append(state.keptIDs, state.rules[len(entries)].ID)
		}
		entries = append(entries, entry)
	}
	writable, readOnly := true, ""
	switch {
	case !s.permitted(permissionWrite):
		writable, readOnly = false, "This instance has no write permission."
	case s.lifecycleWriteRefusal() != "":
		writable, readOnly = false, s.lifecycleWriteRefusal()
	case strings.Contains(state.config.RootName, ":"):
		writable, readOnly = false, "The configuration uses an XML namespace prefix that the editor does not write."
	}
	state.editor = lifecycleEditorState{
		Hash: lifecycleHash(document.Body, editable), Entries: entries, Fields: fields, Hidden: hidden,
		Provider: flavour, ProviderNote: lifecycleProviderNotes[flavour], Versioning: versioning, RootPrefix: rootPrefix,
		Writable: writable, ReadOnlyReason: readOnly, MaxDays: maxLifecycleDays, MaxRules: maxLifecycleRules,
	}
	return state, nil
}

// lifecycleDocumentFor gives the document of a save: the start tag of the root as the provider sent it, the rules of
// the editor, then each other element of the configuration with its bytes. It gives nil when no element stays.
func lifecycleDocumentFor(state lifecycleState, rules []lifecycleEditRule, rootPrefix string) []byte {
	kept := make([]lifecycleItem, 0, len(state.config.Items))
	ruleIndex := 0
	for _, item := range state.config.Items {
		if item.Node.Name == "Rule" {
			_, editable := state.edit[ruleIndex]
			ruleIndex++
			if editable {
				continue
			}
		}
		kept = append(kept, item)
	}
	if len(rules) == 0 && len(kept) == 0 {
		return nil
	}
	var buffer bytes.Buffer
	if len(state.config.RootStart) > 0 && state.config.RootName != "" {
		buffer.Write(state.config.RootStart)
	} else {
		buffer.WriteString(`<LifecycleConfiguration xmlns="` + lifecycleNamespace + `">`)
	}
	unchanged := map[string]bool{}
	for _, rule := range state.edit {
		unchanged[rule.ID] = true
	}
	for _, old := range state.edit {
		for _, rule := range rules {
			if rule.ID == old.ID && !sameEditRule(rule, old) {
				unchanged[old.ID] = false
			}
		}
	}
	for _, rule := range rules {
		if raw, ok := state.raw[rule.ID]; ok && unchanged[rule.ID] {
			buffer.Write(raw)
			continue
		}
		buffer.Write(lifecycleRuleXML(rule, rootPrefix+rule.Prefix))
	}
	for _, item := range kept {
		buffer.Write(item.Raw)
	}
	name := state.config.RootName
	if len(state.config.RootStart) == 0 || name == "" {
		name = "LifecycleConfiguration"
	}
	buffer.WriteString("</" + name + ">")
	return buffer.Bytes()
}

// --- Read-back ------------------------------------------------------------------------------------------------------

// lifecycleWarnings compares the rules that the server sent with the rules that the provider gives after the save,
// and gives a sentence for each difference. It marks each field that the provider dropped.
func (s *storageInstance) lifecycleWarnings(sent []byte, read lifecycleState, rules []lifecycleEditRule) []string {
	warnings := []string{}
	sentConfig, err := splitLifecycleDocument(sent)
	if err != nil {
		return []string{"Stowr cannot read the document that it sent: compare the rules with care."}
	}
	edited := map[string]lifecycleEditRule{}
	for _, rule := range rules {
		edited[rule.ID] = rule
	}
	readRules := []*xmlNode{}
	for _, item := range read.config.Items {
		if item.Node.Name == "Rule" {
			readRules = append(readRules, item.Node)
		}
	}
	matched := make([]bool, len(readRules))
	find := func(node *xmlNode) int {
		id := ""
		if child := node.child("ID"); child != nil {
			id = child.Text
		}
		for index, candidate := range readRules {
			if matched[index] {
				continue
			}
			if child := candidate.child("ID"); id != "" && child != nil && child.Text == id {
				return index
			}
		}
		// A rule without an ID: the same rule, or the same rule with an ID that the provider gave (MinIO).
		for index, candidate := range readRules {
			if !matched[index] && id == "" && withoutID(candidate).canonical() == node.canonical() {
				return index
			}
		}
		return -1
	}
	for _, item := range sentConfig.Items {
		if item.Node.Name != "Rule" {
			continue
		}
		id := ""
		if child := item.Node.child("ID"); child != nil {
			id = child.Text
		}
		index := find(item.Node)
		if index < 0 {
			warnings = append(warnings, fmt.Sprintf("The storage provider did not keep the rule %s.", ruleName(id)))
			continue
		}
		matched[index] = true
		got := readRules[index]
		if got.canonical() == item.Node.canonical() || (id == "" && withoutID(got).canonical() == item.Node.canonical()) {
			continue
		}
		rule, isEdited := edited[id]
		if !isEdited {
			warnings = append(warnings, fmt.Sprintf("The storage provider changed the rule %s, which the editor keeps as is.", ruleName(id)))
			continue
		}
		warnings = append(warnings, s.ruleDifferences(rule, got)...)
	}
	for index, node := range readRules {
		if !matched[index] {
			id := ""
			if child := node.child("ID"); child != nil {
				id = child.Text
			}
			warnings = append(warnings, fmt.Sprintf("The storage provider has a rule %s that Stowr did not send.", ruleName(id)))
		}
	}
	return warnings
}

func withoutID(node *xmlNode) *xmlNode {
	copied := *node
	copied.Children = nil
	for _, child := range node.Children {
		if child.Name != "ID" {
			copied.Children = append(copied.Children, child)
		}
	}
	return &copied
}

// ruleDifferences tells what the provider changed in a rule of the editor.
func (s *storageInstance) ruleDifferences(sent lifecycleEditRule, node *xmlNode) []string {
	name := ruleName(sent.ID)
	got, reason := lifecycleEditRuleFromNode(node, "")
	if reason != "" && reason != lifecycleReasonValues {
		// The fields of the rule are not complete: the server tells the change and marks no field.
		return []string{fmt.Sprintf("The storage provider changed the rule %s: %s", name, strings.ToLower(reason[:1])+reason[1:])}
	}
	sentPrefix := s.cfg.RootPrefix + sent.Prefix
	scope := "every object of the bucket"
	if got.Prefix != "" {
		scope = "every object under " + got.Prefix
	}
	var warnings []string
	if sent.Tag != nil && got.Tag == nil {
		s.markLifecycleDropped(lifecycleFieldTag)
		warnings = append(warnings, fmt.Sprintf("The storage provider dropped the tag filter %s=%s of the rule %s. The rule now applies to %s: disable or delete the rule if you do not want this.", sent.Tag.Key, sent.Tag.Value, name, scope))
	}
	if got.Prefix != sentPrefix {
		warnings = append(warnings, fmt.Sprintf("The storage provider changed the prefix of the rule %s from %q to %q. The rule now applies to %s.", name, sentPrefix, got.Prefix, scope))
	}
	if got.Enabled != sent.Enabled {
		warnings = append(warnings, fmt.Sprintf("The storage provider changed the status of the rule %s.", name))
	}
	if got.ExpirationDays != sent.ExpirationDays {
		warnings = append(warnings, fmt.Sprintf("The storage provider changed the expiration of the rule %s from %d to %d days.", name, sent.ExpirationDays, got.ExpirationDays))
	}
	for _, change := range []struct {
		field     string
		sent, got bool
	}{
		{lifecycleFieldNoncurrent, sent.NoncurrentDays > 0, got.NoncurrentDays > 0},
		{lifecycleFieldAbort, sent.AbortMultipartDays > 0, got.AbortMultipartDays > 0},
		{lifecycleFieldDeleteMarker, sent.ExpiredDeleteMarker, got.ExpiredDeleteMarker},
	} {
		if change.sent && !change.got {
			s.markLifecycleDropped(change.field)
			warnings = append(warnings, fmt.Sprintf("The storage provider dropped %s of the rule %s.", lifecycleFieldNames[change.field], name))
		}
	}
	if got.NoncurrentDays > 0 && sent.NoncurrentDays > 0 && got.NoncurrentDays != sent.NoncurrentDays {
		warnings = append(warnings, fmt.Sprintf("The storage provider changed the days of the noncurrent versions of the rule %s.", name))
	}
	if got.AbortMultipartDays > 0 && sent.AbortMultipartDays > 0 && got.AbortMultipartDays != sent.AbortMultipartDays {
		warnings = append(warnings, fmt.Sprintf("The storage provider changed the days of the abort of incomplete uploads of the rule %s.", name))
	}
	if len(warnings) == 0 {
		warnings = append(warnings, fmt.Sprintf("The storage provider changed the rule %s.", name))
	}
	return warnings
}

// --- Save -----------------------------------------------------------------------------------------------------------

// upstreamStatusText gives the status and the S3 error code of a provider answer, such as "HTTP 403 AccessDenied".
func upstreamStatusText(upstream *upstreamError) string {
	if code := safeProviderErrorCode(upstream.Code); code != "" {
		return fmt.Sprintf("HTTP %d %s", upstream.StatusCode, code)
	}
	return fmt.Sprintf("HTTP %d", upstream.StatusCode)
}

// lifecycleWriteError turns the refusal of a write into the answer of the API. A 403, 501 or 405 makes the editor of
// the instance read only, with the reason.
func (s *storageInstance) lifecycleWriteError(err error) error {
	var upstream *upstreamError
	if !errors.As(err, &upstream) {
		return err
	}
	switch {
	case providerRefusedFeature(err):
		reason := fmt.Sprintf("The storage provider does not let Stowr change the lifecycle rules (%s).", upstreamStatusText(upstream))
		s.setLifecycleWriteRefusal(reason)
		return apiError{Status: http.StatusNotImplemented, Code: "lifecycle_write_unsupported", Message: reason}
	case upstream.StatusCode == http.StatusForbidden:
		reason := fmt.Sprintf("The storage provider refused the change (%s): the credentials of this instance cannot change the lifecycle rules.", upstreamStatusText(upstream))
		s.setLifecycleWriteRefusal(reason)
		return apiError{Status: http.StatusForbidden, Code: "lifecycle_write_denied", Message: reason}
	case upstream.StatusCode >= 400 && upstream.StatusCode < 500:
		return apiError{Status: http.StatusBadRequest, Code: "lifecycle_refused", Message: fmt.Sprintf("The storage provider refused the rules (%s). The rules of the bucket did not change.", upstreamStatusText(upstream))}
	}
	return err
}

// SaveLifecycle checks the hash, writes the configuration with the rules of the editor and the kept elements, and
// reads it again. It gives the new state and a sentence for each difference between the sent and the kept rules.
func (s *storageInstance) SaveLifecycle(ctx context.Context, hash string, rules []lifecycleEditRule) (lifecycleState, []string, error) {
	writer, err := s.lifecycleWriter()
	if err != nil {
		return lifecycleState{}, nil, err
	}
	s.lifecycle.write.Lock()
	defer s.lifecycle.write.Unlock()
	state, err := s.readLifecycleState(ctx)
	if err != nil {
		return lifecycleState{}, nil, err
	}
	if state.editor.Hash != hash {
		return lifecycleState{}, nil, lifecycleChangedError()
	}
	if strings.Contains(state.config.RootName, ":") {
		return lifecycleState{}, nil, apiError{Status: http.StatusConflict, Code: "lifecycle_read_only", Message: state.editor.ReadOnlyReason}
	}
	if err := checkLifecycleRules(state, rules); err != nil {
		return lifecycleState{}, nil, err
	}
	body := lifecycleDocumentFor(state, rules, s.cfg.RootPrefix)
	release, err := s.beginStorageRequest(ctx)
	if err != nil {
		return lifecycleState{}, nil, err
	}
	if body == nil {
		err = writer.DeleteBucketLifecycle(ctx)
	} else {
		err = writer.WriteBucketLifecycle(ctx, body)
	}
	release()
	s.observePermission(permissionWrite, err)
	if err != nil {
		return lifecycleState{}, nil, s.lifecycleWriteError(err)
	}
	s.setLifecycleWriteRefusal("")
	read, err := s.readLifecycleState(ctx)
	if err != nil {
		// The write worked: the answer must not say that the save failed. The page reads the rules again.
		return lifecycleState{}, []string{fmt.Sprintf("The rules are saved, but Stowr cannot read them again (%s): read them again before another change.", publicStorageError(err))}, nil
	}
	warnings := s.lifecycleWarnings(body, read, rules)
	if len(warnings) > 0 {
		// A field that the provider dropped is no longer offered: the state of the answer follows.
		if again, err := s.readLifecycleState(ctx); err == nil {
			read = again
		}
	}
	return read, warnings, nil
}

// checkLifecycleRules checks the rules of a save against the state that the server read: the fields that the editor
// offers, unique IDs (also with the kept rules), and the limit of rules.
func checkLifecycleRules(state lifecycleState, rules []lifecycleEditRule) error {
	ids := map[string]bool{}
	kept := len(state.keptIDs)
	for _, id := range state.keptIDs {
		if id != "" {
			ids[id] = true
		}
	}
	if kept+len(rules) > maxLifecycleRules {
		return invalidLifecycle("a bucket has at most %d lifecycle rules, got %d", maxLifecycleRules, kept+len(rules))
	}
	for _, rule := range rules {
		if err := validateLifecycleEditRule(rule); err != nil {
			return err
		}
		if ids[rule.ID] {
			return invalidLifecycle("the ID %s is on more than one rule", ruleName(rule.ID))
		}
		ids[rule.ID] = true
		for _, field := range rule.fields() {
			if !state.editor.Fields[field] {
				reason := ""
				for _, hidden := range state.editor.Hidden {
					if hidden.Field == field {
						reason = " " + hidden.Reason
					}
				}
				return apiError{Status: http.StatusBadRequest, Code: "lifecycle_field_unsupported",
					Message: fmt.Sprintf("the rule %s uses %s, which the editor does not offer for this instance.%s", ruleName(rule.ID), lifecycleFieldNames[field], reason)}
			}
		}
	}
	return nil
}

// --- HTTP -----------------------------------------------------------------------------------------------------------

type lifecycleRequest struct {
	Instance string              `json:"instance"`
	Hash     string              `json:"hash"`
	Rules    []lifecycleEditRule `json:"rules"`
}

var lifecycleHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// lifecycleDeletes tells if the save adds or changes a rule that deletes objects. Such a save needs the permission
// delete of the bucket.
func lifecycleDeletes(state lifecycleState, rules []lifecycleEditRule) bool {
	old := map[string]lifecycleEditRule{}
	for _, rule := range state.edit {
		old[rule.ID] = rule
	}
	for _, rule := range rules {
		previous, existed := old[rule.ID]
		if existed && sameEditRule(previous, rule) {
			continue
		}
		if rule.destructive() {
			return true
		}
	}
	return false
}

func (a *application) handleLifecycleSave(w http.ResponseWriter, r *http.Request) {
	var request lifecycleRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeAPIError(w, err)
		return
	}
	instance, err := a.instanceFromRequest(r, request.Instance)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	for _, permission := range []string{permissionRead, permissionWrite} {
		if err := requirePermission(instance, permission); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	if !lifecycleHashPattern.MatchString(request.Hash) {
		writeAPIError(w, invalidLifecycle("hash must be the hash that GET /api/lifecycle gave"))
		return
	}
	if request.Rules == nil {
		request.Rules = []lifecycleEditRule{}
	}
	for index := range request.Rules {
		request.Rules[index].Prefix = cleanRelativeKey(request.Rules[index].Prefix)
	}
	if _, err := instance.lifecycleWriter(); err != nil {
		writeAPIError(w, err)
		return
	}
	// The access check needs the rules that the save removes: a first read gives them. The save reads again.
	current, err := instance.readLifecycleState(r.Context())
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if lifecycleDeletes(current, request.Rules) {
		if err := requirePermission(instance, permissionDelete); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	if _, err := a.authorize(r, a.lifecycleOperation(instance)); err != nil {
		writeAPIError(w, err)
		return
	}
	state, warnings, err := instance.SaveLifecycle(r.Context(), request.Hash, request.Rules)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	response := lifecycleResponse{Instance: instance.cfg.ID, Rules: state.rules, Editor: &state.editor, Warnings: warnings}
	if state.editor.Hash == "" {
		// The read-back failed: the answer has no state, and the page reads the rules again.
		response.Rules, response.Editor = []lifecycleRule{}, nil
	}
	writeJSON(w, http.StatusOK, response)
}
