package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The options route of the service (ProcurAuth 5.6 to 5.8): GET /.well-known/procurauth/resources. It lists the
// children of a node, page by page. It shows only the children that the subject of the identity token can see.
//
// The route asks the PDP one question for each child that it can show: the match "subtree", no usage (ProcurAuth 4.4,
// 10.1). It asks the actions in this order: object.read, object.write, object.delete, object.lock and bucket.admin.
// A child is visible when one answer is true. The route asks only the actions that the action document publishes for
// the type of the child. It stops to ask for a child at the first true answer.
//
// The rules that the service chooses (ProcurAuth 5.6, 5.7):
//   - q for the buckets is a substring of the id or of the label, without case.
//   - q for the folders and the subfolders is a prefix of the id, with case. The storage can search only by prefix.
//   - A request asks at most optionsMaxQuestions questions. When this bound stops the walk, the answer holds the
//     items found so far and a cursor. The answer can have no item and a cursor.
//   - A node that does not exist gives an answer with no items.

const (
	accessOptionsPath = "/.well-known/procurauth/resources"

	// optionsMaxItems is the most items of one answer (ProcurAuth 5.8).
	optionsMaxItems = 50
	// optionsMaxQuestions is the most questions to the PDP for one request (ProcurAuth 5.6).
	optionsMaxQuestions = 200
	// optionsMaxQueryBytes is the longest raw query that the route reads.
	optionsMaxQueryBytes = 16 << 10
	// optionsMaxSearchBytes is the longest value of q.
	optionsMaxSearchBytes = 256
	// optionsMaxCursorBytes is the longest text that a cursor holds before its encoding.
	optionsMaxCursorBytes = 2048
	// optionsProviderPage is the number of keys that one request to the storage asks for.
	optionsProviderPage = 1000
	// optionsMaxLogParent is the longest parent that the log line shows.
	optionsMaxLogParent = 200

	optionsErrorCode = "invalid_options_request"
)

// accessOptionsSource is what the options route reads. The router fills it from the application.
type accessOptionsSource struct {
	Service   string // access.identity.audience
	Doc       accessDocument
	PDP       *pdpClient
	Instances []*storageInstance // the bucket blocks then the volume blocks, in the order of the configuration
	LogMode   string
}

// optionsActionOrder is the order of the actions that the route asks for each child.
var optionsActionOrder = []string{actionObjectRead, actionObjectWrite, actionObjectDelete, actionObjectLock, actionBucketAdmin}

// optionsItem is one child in the answer.
type optionsItem struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	HasChildren bool   `json:"has_children"`
}

// optionsAnswer is the JSON object of the answer.
type optionsAnswer struct {
	Items []optionsItem `json:"items"`
	Next  *string       `json:"next"`
}

// optionsRequest is the request after the check of its parameters.
type optionsRequest struct {
	childType string
	// parent is the chain of the nodes below the service, from the bucket.
	parent []accessNode
	// parentText is the value of the parameter parent, decoded one time. It is for the log.
	parentText string
	// missingParent is true when the parent cannot exist: one of its ids cannot be the id of a node.
	missingParent bool
	search        string
	limit         int
	// after is the payload of the cursor. It is valid only when hasAfter is true.
	after    string
	hasAfter bool
}

func optionsBadRequest(message string) error {
	return apiError{Status: http.StatusBadRequest, Code: optionsErrorCode, Message: message}
}

// optionsChildType gives the type of the children of a node. The depth is the number of nodes of the path of the node
// below the service: 0 is the service, 1 is a bucket, 2 is a folder, 3 is a subfolder. It gives "" for a subfolder.
func optionsChildType(depth int) string {
	switch depth {
	case 0:
		return accessTypeBucket
	case 1, 2:
		return accessFolderTypes[depth-1]
	}
	return ""
}

// parseOptionsQuery reads the raw query as ProcurAuth 5.7 says. The function splits the query at "&" and at the first
// "=" of each pair. It decodes each name and each value one time with url.PathUnescape: this keeps "+" as it is and
// refuses a bad escape. A parameter that the route does not know is ignored, also its value.
func parseOptionsQuery(rawQuery string) (optionsRequest, error) {
	var req optionsRequest
	if len(rawQuery) > optionsMaxQueryBytes {
		return req, optionsBadRequest("the query is too long")
	}
	values := make(map[string]string, 5)
	for _, pair := range strings.Split(rawQuery, "&") {
		if pair == "" {
			continue
		}
		rawName, rawValue, _ := strings.Cut(pair, "=")
		name, err := url.PathUnescape(rawName)
		if err != nil {
			continue
		}
		switch name {
		case "parent", "type", "q", "limit", "after":
		default:
			continue
		}
		if _, seen := values[name]; seen {
			return req, optionsBadRequest("the parameter " + name + " occurs more than one time")
		}
		value, err := url.PathUnescape(rawValue)
		if err != nil {
			return req, optionsBadRequest("the parameter " + name + " has a bad percent-encoding")
		}
		values[name] = value
	}

	req.childType = values["type"]
	if req.childType == "" {
		return req, optionsBadRequest("the parameter type is mandatory")
	}
	if text, present := values["parent"]; present {
		if err := req.parseParent(text); err != nil {
			return req, err
		}
	}
	if want := optionsChildType(len(req.parent)); req.childType != want {
		if want == "" {
			return req, optionsBadRequest("the parent has no children")
		}
		return req, optionsBadRequest("the parameter type must be " + want + " for this parent")
	}
	if text, present := values["limit"]; present {
		limit, err := parseOptionsLimit(text)
		if err != nil {
			return req, err
		}
		req.limit = limit
	} else {
		req.limit = optionsMaxItems
	}
	if text, present := values["q"]; present {
		if len(text) > optionsMaxSearchBytes {
			return req, optionsBadRequest("the parameter q is too long")
		}
		req.search = text
	}
	if text, present := values["after"]; present {
		payload, ok := decodeOptionsCursor(text, req.childType)
		if !ok {
			return req, optionsBadRequest("the parameter after is not a cursor of this route")
		}
		req.after, req.hasAfter = payload, true
	}
	return req, nil
}

// parseParent reads the value of the parameter parent: a path of nodes below the service, for example
// "bucket:b/folder:f". The value is already decoded one time. The function splits it at each "/" and decodes each id.
func (req *optionsRequest) parseParent(text string) error {
	req.parentText = text
	if text == "" {
		return optionsBadRequest("the parameter parent is empty")
	}
	for index, part := range strings.Split(text, "/") {
		kind, rawID, found := strings.Cut(part, ":")
		if !found || kind == "" || rawID == "" {
			return optionsBadRequest("the parameter parent must have the form <type>:<id> for each node")
		}
		if want := optionsChildType(index); kind != want {
			if want == "" {
				return optionsBadRequest("the parameter parent has too many nodes")
			}
			return optionsBadRequest("the parameter parent must have the type " + want + " at position " + strconv.Itoa(index+1))
		}
		id, err := url.PathUnescape(rawID)
		if err != nil {
			return optionsBadRequest("the parameter parent has a bad percent-encoding in an id")
		}
		if id == "" {
			return optionsBadRequest("the parameter parent has an empty id")
		}
		// An id that is not valid cannot be the id of a node that exists. The id of a folder never holds "/".
		if !validAccessID(id) || (index > 0 && strings.Contains(id, "/")) {
			req.missingParent = true
		}
		req.parent = append(req.parent, accessNode{Type: kind, ID: id})
	}
	return nil
}

// parseOptionsLimit reads the parameter limit: a whole number of digits only, from 1. A number above the maximum gives
// the maximum. The service can give fewer items than the limit (ProcurAuth 5.7).
func parseOptionsLimit(text string) (int, error) {
	if text == "" {
		return 0, optionsBadRequest("the parameter limit must be a whole number from 1")
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return 0, optionsBadRequest("the parameter limit must be a whole number from 1")
		}
	}
	digits := strings.TrimLeft(text, "0")
	if digits == "" {
		return 0, optionsBadRequest("the parameter limit must be a whole number from 1")
	}
	if len(digits) > 6 {
		return optionsMaxItems, nil
	}
	value, _ := strconv.Atoi(digits)
	if value > optionsMaxItems {
		value = optionsMaxItems
	}
	return value, nil
}

// encodeOptionsCursor makes the cursor: the type of the children, a zero byte and the payload, in base64url. The
// payload for the buckets is the id of the last bucket that the walk examined. The payload for the folders is a key,
// relative to the parent: the listing of the storage continues after it.
func encodeOptionsCursor(childType, payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(childType + "\x00" + payload))
}

// decodeOptionsCursor reads a cursor. It gives false when the text is not a cursor that this route made for this type.
func decodeOptionsCursor(text, childType string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return "", false
	}
	kind, payload, found := strings.Cut(string(raw), "\x00")
	if !found || kind != childType || payload == "" || len(payload) > optionsMaxCursorBytes || !utf8.ValidString(payload) {
		return "", false
	}
	return payload, true
}

// optionsLabel cuts a label to 60 characters (ProcurAuth 5.8).
func optionsLabel(text string) string {
	if utf8.RuneCountInString(text) <= maxAccessLabelRunes {
		return text
	}
	runes := []rune(text)
	return string(runes[:maxAccessLabelRunes])
}

// optionsCandidate is one child that the route can ask the PDP about.
type optionsCandidate struct {
	ID          string
	Label       string
	HasChildren bool
}

// optionsCandidates gives the children of one node, in order, without the PDP.
type optionsCandidates interface {
	// take gives the next candidates, n at most. It gives fewer when no more are at hand.
	take(ctx context.Context, n int) ([]optionsCandidate, error)
	// cursor gives the payload of the cursor after the candidates that take gave, and tells if more candidates can
	// exist after them.
	cursor() (payload string, more bool)
}

// bucketCandidates are the bucket blocks and the volume blocks.
type bucketCandidates struct {
	list     []optionsCandidate
	index    int
	lastID   string
	consumed bool
	after    string
}

// sortKeyLess compares two ids in the order of the buckets: the lower-case id, then the id.
func sortKeyLess(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

// newBucketCandidates sorts the instances by id and keeps those that q finds and that follow the cursor.
func newBucketCandidates(instances []*storageInstance, search string, after string, hasAfter bool) *bucketCandidates {
	needle := strings.ToLower(search)
	list := make([]optionsCandidate, 0, len(instances))
	for _, instance := range instances {
		id := instance.cfg.ID
		if !validAccessID(id) {
			continue
		}
		label := id
		if name := instance.cfg.Name; utf8.ValidString(name) && !hasControlCharacter(name) {
			if count := utf8.RuneCountInString(name); count >= 1 && count <= maxAccessLabelRunes {
				label = name
			}
		}
		if needle != "" && !strings.Contains(strings.ToLower(id), needle) && !strings.Contains(strings.ToLower(label), needle) {
			continue
		}
		list = append(list, optionsCandidate{ID: id, Label: optionsLabel(label), HasChildren: true})
	}
	sort.SliceStable(list, func(i, j int) bool { return sortKeyLess(list[i].ID, list[j].ID) })
	start := 0
	if hasAfter {
		start = sort.Search(len(list), func(i int) bool { return sortKeyLess(after, list[i].ID) })
	}
	return &bucketCandidates{list: list[start:], after: after}
}

func (b *bucketCandidates) take(_ context.Context, n int) ([]optionsCandidate, error) {
	end := b.index + n
	if end > len(b.list) {
		end = len(b.list)
	}
	out := b.list[b.index:end]
	b.index = end
	if len(out) > 0 {
		b.lastID, b.consumed = out[len(out)-1].ID, true
	}
	return out, nil
}

func (b *bucketCandidates) cursor() (string, bool) {
	payload := b.after
	if b.consumed {
		payload = b.lastID
	}
	return payload, b.index < len(b.list)
}

// folderCandidates are the folders or the subfolders below one node of a storage instance. They come from the listing
// of the storage with the delimiter "/", with the own credentials of the service.
type folderCandidates struct {
	instance *storageInstance
	// parentRel is "" for the children of a bucket, or the relative path of the parent with "/" at the end.
	parentRel string
	search    string
	// startAfter is the cursor payload, relative to the parent.
	startAfter  string
	hasChildren bool

	buffer    []string
	pageToken string
	started   bool
	exhausted bool
	pages     int
	maxPages  int
	// high is the largest key, relative to the parent, that the walk read. It is a valid start for the next request
	// when the walk consumed all that it read.
	high         string
	consumedLast string
	consumed     bool
}

func newFolderCandidates(instance *storageInstance, parentRel, search, after string, hasChildren bool) *folderCandidates {
	maxPages := instance.cfg.MaxScanPages
	if maxPages < 1 {
		maxPages = 1
	}
	return &folderCandidates{
		instance: instance, parentRel: parentRel, search: search, startAfter: after,
		hasChildren: hasChildren, maxPages: maxPages,
	}
}

// skipKey gives the key to use as "start after" so that a listing skips the folder with this id and each key in it.
// All the keys of the folder start with "id/". The character "0" follows "/" in the order of the bytes.
func skipKey(id string) string { return id + "0" }

// fetch reads one page of the storage and adds its folders to the buffer.
func (f *folderCandidates) fetch(ctx context.Context) error {
	options := listOptions{
		Prefix:     f.instance.fullKey(f.parentRel + f.search),
		Delimiter:  "/",
		MaxResults: optionsProviderPage,
		PageToken:  f.pageToken,
	}
	if f.startAfter != "" {
		options.StartAfter = f.instance.fullKey(f.parentRel + f.startAfter)
	}
	page, err := f.instance.List(ctx, options)
	if err != nil {
		return err
	}
	f.pages++
	f.started = true
	f.pageToken = page.NextPageToken
	if f.pageToken == "" {
		f.exhausted = true
	}
	raise := func(key string) {
		if key > f.high {
			f.high = key
		}
	}
	for _, full := range page.Prefixes {
		relative, ok := f.instance.relativeKey(full)
		if !ok || !strings.HasPrefix(relative, f.parentRel) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(relative, f.parentRel), "/")
		if name != "" && !strings.Contains(name, "/") {
			raise(skipKey(name))
		}
		// A part that is not a valid id cannot be a node: the route skips it.
		if name == "" || strings.Contains(name, "/") || !validAccessID(name) {
			continue
		}
		f.buffer = append(f.buffer, name)
	}
	for _, object := range page.Objects {
		relative, ok := f.instance.relativeKey(object.Key)
		if ok && strings.HasPrefix(relative, f.parentRel) {
			raise(strings.TrimPrefix(relative, f.parentRel))
		}
	}
	return nil
}

func (f *folderCandidates) take(ctx context.Context, n int) ([]optionsCandidate, error) {
	for len(f.buffer) < n && !f.exhausted && f.pages < f.maxPages {
		if err := f.fetch(ctx); err != nil {
			return nil, err
		}
	}
	if n > len(f.buffer) {
		n = len(f.buffer)
	}
	out := make([]optionsCandidate, 0, n)
	for _, id := range f.buffer[:n] {
		out = append(out, optionsCandidate{ID: id, Label: optionsLabel(id), HasChildren: f.hasChildren})
	}
	f.buffer = f.buffer[n:]
	if n > 0 {
		f.consumedLast, f.consumed = out[n-1].ID, true
	}
	return out, nil
}

func (f *folderCandidates) cursor() (string, bool) {
	more := len(f.buffer) > 0 || (f.started && !f.exhausted)
	payload := f.startAfter
	if f.consumed {
		payload = skipKey(f.consumedLast)
	}
	if len(f.buffer) == 0 && f.high > payload {
		// The walk read more keys than it consumed, and none of them is a candidate that it keeps.
		payload = f.high
	}
	return payload, more
}

// optionsActionsFor gives the published actions whose resource types hold the type of the child, in the order of the
// questions.
func optionsActionsFor(doc accessDocument, childType string) []string {
	var out []string
	for _, action := range optionsActionOrder {
		if doc.published(action) && containsString(doc.actions[action].ResourceTypes, childType) {
			out = append(out, action)
		}
	}
	return out
}

// optionsWalk asks the PDP about the candidates and gives the visible ones, limit at most. It gives the cursor of the
// next page, or "" for the last page. questions is the number of questions that it asked. A failed answer gives
// failed = true.
func optionsWalk(ctx context.Context, src accessOptionsSource, subject, requestID, childType string, ancestors []accessNode,
	candidates optionsCandidates, limit int) (items []optionsItem, next string, questions int, failed bool, err error) {
	items = []optionsItem{}
	actions := optionsActionsFor(src.Doc, childType)
	if len(actions) == 0 {
		return items, "", 0, false, nil
	}
	for len(items) < limit {
		// The chunk is small enough for the worst case: each child needs one question for each action.
		size := limit - len(items)
		if room := (optionsMaxQuestions - questions) / len(actions); room < size {
			size = room
		}
		if size < 1 {
			break
		}
		chunk, err := candidates.take(ctx, size)
		if err != nil {
			return nil, "", questions, false, err
		}
		if len(chunk) == 0 {
			break
		}
		visible := make([]bool, len(chunk))
		pending := make([]int, len(chunk))
		for i := range pending {
			pending[i] = i
		}
		for _, action := range actions {
			if len(pending) == 0 {
				break
			}
			asked := make([]pdpQuestion, len(pending))
			for i, index := range pending {
				asked[i] = pdpQuestion{
					Action:  action,
					Subtree: true,
					Resource: accessResource{
						Type: childType, ID: chunk[index].ID, Ancestors: ancestors,
					},
				}
			}
			answers := src.PDP.decide(ctx, requestID, subject, asked)
			questions += len(asked)
			still := pending[:0]
			for i, index := range pending {
				if answers[i].Failed {
					return nil, "", questions, true, nil
				}
				if answers[i].Decision {
					visible[index] = true
				} else {
					still = append(still, index)
				}
			}
			pending = still
		}
		for index, candidate := range chunk {
			if visible[index] {
				items = append(items, optionsItem{ID: candidate.ID, Label: candidate.Label, HasChildren: candidate.HasChildren})
			}
		}
	}
	if payload, more := candidates.cursor(); more {
		next = encodeOptionsCursor(childType, payload)
	}
	return items, next, questions, false, nil
}

// optionsLogParent gives the parent for the log line. The mode "anonymous" keeps the ids of the folders out of the log.
func optionsLogParent(req optionsRequest, logMode string) string {
	if logMode == logModeDetailed {
		text := req.parentText
		if len(text) > optionsMaxLogParent {
			text = text[:optionsMaxLogParent]
		}
		return text
	}
	parts := make([]string, 0, len(req.parent))
	for index, node := range req.parent {
		if index == 0 {
			parts = append(parts, node.Type+":"+percentEncodeID(node.ID))
		} else {
			parts = append(parts, node.Type+":-")
		}
	}
	text := strings.Join(parts, "/")
	if len(text) > optionsMaxLogParent {
		text = text[:optionsMaxLogParent]
	}
	return text
}

// serveAccessOptions answers one request. The middleware already checked the identity token: subject and requestID
// come from it. The function never reads the identity header.
func serveAccessOptions(w http.ResponseWriter, r *http.Request, src accessOptionsSource, subject, requestID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	if src.PDP == nil {
		http.NotFound(w, r)
		return
	}
	var (
		req       optionsRequest
		count     int
		questions int
	)
	typeText := "-"
	defer func() {
		log.Printf("access.options request_id=%s subject=%q type=%s parent=%q items=%d questions=%d",
			pdpLogToken(requestID), subject, typeText, optionsLogParent(req, src.LogMode), count, questions)
	}()

	req, err := parseOptionsQuery(r.URL.RawQuery)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	typeText = req.childType

	empty := func() {
		writeOptionsAnswer(w, optionsAnswer{Items: []optionsItem{}})
	}
	var (
		candidates optionsCandidates
		ancestors  = []accessNode{{Type: accessTypeService, ID: src.Service}}
	)
	if req.childType == accessTypeBucket {
		candidates = newBucketCandidates(src.Instances, req.search, req.after, req.hasAfter)
	} else {
		if req.missingParent {
			empty()
			return
		}
		var instance *storageInstance
		for _, candidate := range src.Instances {
			if candidate.cfg.ID == req.parent[0].ID {
				instance = candidate
				break
			}
		}
		if instance == nil {
			empty()
			return
		}
		ancestors = append(ancestors, req.parent...)
		if requirePermission(instance, permissionRead) != nil {
			// The storage cannot give the folders when the process may not read.
			empty()
			return
		}
		// No id of a folder holds "/": a search that holds "/" finds nothing.
		if strings.Contains(req.search, "/") {
			empty()
			return
		}
		parentRel := ""
		for _, node := range req.parent[1:] {
			parentRel += node.ID + "/"
		}
		candidates = newFolderCandidates(instance, parentRel, req.search, req.after, req.childType == accessTypeFolder)
	}

	items, next, asked, failed, err := optionsWalk(r.Context(), src, subject, requestID, req.childType, ancestors, candidates, req.limit)
	questions = asked
	if failed {
		writeAPIError(w, apiError{Status: http.StatusServiceUnavailable, Code: "pdp_unavailable", Message: "the access service gave no valid answer"})
		return
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}
	count = len(items)
	answer := optionsAnswer{Items: items}
	if next != "" {
		answer.Next = &next
	}
	writeOptionsAnswer(w, answer)
}

// writeOptionsAnswer writes the answer. The size stays below 64 KiB: an item has an id of 256 bytes at most and a label
// of 60 characters at most, and an answer has 50 items at most (ProcurAuth 5.8).
func writeOptionsAnswer(w http.ResponseWriter, answer optionsAnswer) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(answer); err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(body.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}

// handleAccessOptions serves the options route. The middleware checked the identity token before this method runs.
// The route has no action of its own: it asks only the subtree questions of ProcurAuth 5.6.
func (a *application) handleAccessOptions(w http.ResponseWriter, r *http.Request) {
	subject, requestID := "", ""
	if request := accessFromContext(r.Context()); request != nil {
		subject, requestID = request.Subject, request.RequestID
	}
	instances := make([]*storageInstance, 0, len(a.order))
	for _, id := range a.order {
		instances = append(instances, a.instances[id])
	}
	serveAccessOptions(w, r, accessOptionsSource{
		Service: a.config.Access.Audience, Doc: a.accessDoc, PDP: a.pdp,
		Instances: instances, LogMode: a.config.Runtime.LogMode,
	}, subject, requestID)
}
