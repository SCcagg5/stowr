package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"sort"
	"time"
)

// The access middleware, the route table and the central authorize helper (ProcurAuth 4, 6, 7 and 10).
//
// The middleware reads the identity token and finds the subject. The handler of a route asks the PDP through
// authorize, after it checks the instance, the shape of the request and the process ceiling.

const (
	codeUnauthenticated = "unauthenticated"
	codeForbidden       = "forbidden"
	codeTooManyRequests = "too_many_requests"
	codePDPUnavailable  = "pdp_unavailable"

	// accessActionUnknown is the action that the answer shows for a route that has no access rule.
	accessActionUnknown = "unknown"

	maxRequestIDBytes = 128
)

// routeAccess is the kind of access of a route.
type routeAccess int

const (
	// routeDenied is the zero value. The route answers 403 with the action "unknown" and logs access.no_rule.
	routeDenied routeAccess = iota
	// routeExempt needs no identity token: /healthz and the action document.
	routeExempt
	// routeTokenOnly needs the identity token and asks no question: the static files, the pages, /api/build and the
	// 404 of /api/jobs.
	routeTokenOnly
	// routeHandlerAsks needs the identity token. The handler asks the PDP for each operation.
	routeHandlerAsks
)

// routeEntry is one route of the table.
type routeEntry struct {
	pattern string
	access  routeAccess
	handler http.HandlerFunc
}

// guardRoute gives the handler of an entry. An entry without a kind gives the answer 403 and no call to its handler.
func (a *application) guardRoute(entry routeEntry) http.Handler {
	if entry.access != routeDenied {
		return entry.handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := ""
		if request := accessFromContext(r.Context()); request != nil {
			requestID = request.RequestID
		} else {
			requestID = requestIDFrom(r)
		}
		log.Printf("access.no_rule request_id=%s method=%s path=%q",
			requestID, r.Method, loggedRequestPath(a.config.Runtime.LogMode, r.URL.Path))
		writeAPIError(w, errForbidden(accessActionUnknown))
	})
}

// accessExemptPath tells if a path needs no identity token. The middleware reads no token for these two paths.
func accessExemptPath(path string) bool {
	return path == "/healthz" || path == accessActionsPath
}

// handleAccessActions serves the action document.
func (a *application) handleAccessActions(w http.ResponseWriter, r *http.Request) {
	serveAccessActions(w, r, a.config.Access, a.accessDoc)
}

// accessRequest is the identity of a request that passed the middleware.
type accessRequest struct {
	Subject string
	// Owner is for the log only. It can be empty.
	Owner     string
	RequestID string
}

type accessRequestKey struct{}

// accessFromContext gives the identity of the request, or nil when the middleware did not set it.
func accessFromContext(ctx context.Context) *accessRequest {
	value, _ := ctx.Value(accessRequestKey{}).(*accessRequest)
	return value
}

// requestLogInfo lets the access middleware give the subject to the request log.
type requestLogInfo struct {
	Subject   string
	RequestID string
}

type requestLogKey struct{}

func requestLogInfoFromContext(ctx context.Context) *requestLogInfo {
	value, _ := ctx.Value(requestLogKey{}).(*requestLogInfo)
	return value
}

// requestIDFrom keeps a valid X-Request-ID of the client: 1 to 128 characters of A-Z, a-z, 0-9, '.', '_', ':' and
// '-'. Else it makes a random identifier.
func requestIDFrom(r *http.Request) string {
	value := r.Header.Get("X-Request-ID")
	valid := value != "" && len(value) <= maxRequestIDBytes
	for i := 0; valid && i < len(value); i++ {
		c := value[i]
		valid = c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-'
	}
	if valid {
		return value
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(random[:])
}

// errUnauthenticated gives the 401 answer.
func errUnauthenticated() apiError {
	return apiError{Status: http.StatusUnauthorized, Code: codeUnauthenticated, Message: "A valid identity token is required."}
}

// errForbidden gives the 403 answer of an action. The answer never shows the reason of the PDP.
func errForbidden(action string) apiError {
	return apiError{Status: http.StatusForbidden, Code: codeForbidden, Message: "The action " + action + " is not allowed.", Action: action}
}

// errTooManyRequests gives the 429 answer of an action that has used its budget.
func errTooManyRequests(action string, retryAfter int64) apiError {
	return apiError{Status: http.StatusTooManyRequests, Code: codeTooManyRequests, Message: "The budget of the action " + action + " is used.",
		Action: action, RetryAfter: retryAfter}
}

// errPDPUnavailable gives the 503 answer when the PDP gives no valid decision.
func errPDPUnavailable(action string) apiError {
	return apiError{Status: http.StatusServiceUnavailable, Code: codePDPUnavailable, Message: "The decision for the action " + action + " is not available.", Action: action}
}

// accessMiddleware checks the identity token of each request. The token comes only from the configured header. The
// middleware never reads Authorization, a cookie or a query parameter. In mode open it does nothing.
func (a *application) accessMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.config.Access.enforced() || accessExemptPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		requestID := requestIDFrom(r)
		values := r.Header.Values(a.config.Access.Header)
		result := identityResult{Reason: "missing"}
		switch {
		case len(values) > 1:
			// The edge replaces the header (ProcurAuth 9.1): two copies are a malformed request.
			result = identityResult{Reason: "malformed"}
		case len(values) == 1 && a.identity != nil:
			result = a.identity.verify(values[0], time.Now())
		}
		if !result.ok() {
			log.Printf("access.unauthenticated request_id=%s method=%s path=%q reason=%s",
				requestID, r.Method, loggedRequestPath(a.config.Runtime.LogMode, r.URL.Path), result.Reason)
			if !isSafeHTTPMethod(r.Method) {
				w.Header().Set("Connection", "close")
			}
			writeAPIError(w, errUnauthenticated())
			return
		}
		if info := requestLogInfoFromContext(r.Context()); info != nil {
			info.Subject = result.Subject
			info.RequestID = requestID
		}
		ctx := context.WithValue(r.Context(), accessRequestKey{}, &accessRequest{Subject: result.Subject, Owner: result.Owner, RequestID: requestID})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessAsk is one permission: an action on a resource.
type accessAsk struct {
	Action   string
	Resource accessResource
}

// accessOperation is the permissions of one operation. Others come first, without usage. Use comes last: it is the
// permission that the operation uses, and a budget counts it.
//
// Continuation is true when the request continues a use that already took its unit: an upload chunk, an upload status,
// a resume or a cancel, a read or a control of a job, a request of a SQLite session. The use of a continuation never
// carries usage, even when its action lists a budget.
type accessOperation struct {
	Others       []accessAsk
	Use          accessAsk
	Continuation bool
}

// accessGrant is what the PDP gives for an operation: the ceilings, by action and by name.
type accessGrant struct {
	Limits map[string]map[string]int64
}

// objectAsk gives the permission on the node of a key or of a prefix of an instance. rel is the exact relative key or
// prefix that the handler sends to the storage.
func (a *application) objectAsk(action string, instance *storageInstance, rel string) accessAsk {
	return accessAsk{Action: action, Resource: keyResource(a.config.Access.Audience, instance.cfg.ID, rel)}
}

// bucketAsk gives the permission on the bucket node of an instance.
func (a *application) bucketAsk(action string, instance *storageInstance) accessAsk {
	return accessAsk{Action: action, Resource: bucketResource(a.config.Access.Audience, instance.cfg.ID)}
}

// authorizeRead asks object.read on the node of a key or of a prefix.
func (a *application) authorizeRead(r *http.Request, instance *storageInstance, rel string) (accessGrant, error) {
	return a.authorize(r, accessOperation{Use: a.objectAsk(actionObjectRead, instance, rel)})
}

// authorizeReadBucket asks object.read on the bucket node.
func (a *application) authorizeReadBucket(r *http.Request, instance *storageInstance) (accessGrant, error) {
	return a.authorize(r, accessOperation{Use: a.bucketAsk(actionObjectRead, instance)})
}

// authorizeReadMany asks object.read for each key. The first key is the use. The other keys come first.
func (a *application) authorizeReadMany(r *http.Request, instance *storageInstance, keys []string) (accessGrant, error) {
	if len(keys) == 0 {
		return a.authorize(r, accessOperation{Use: a.objectAsk(actionObjectRead, instance, "")})
	}
	op := accessOperation{Use: a.objectAsk(actionObjectRead, instance, keys[0])}
	for _, key := range keys[1:] {
		op.Others = append(op.Others, a.objectAsk(actionObjectRead, instance, key))
	}
	return a.authorize(r, op)
}

// authorize asks the PDP for the permissions of an operation and gives the grant, or an apiError. In mode open it
// asks nothing.
//
// The service asks the Others first, without usage. A refused Other stops the operation before the question of the
// use. When the action of the use lists a budget, the use is one Access Evaluation with usage. Else the use goes in
// the same batch as the Others. A continuation never asks usage: its use goes in the same batch as the Others.
func (a *application) authorize(r *http.Request, op accessOperation) (accessGrant, error) {
	if !a.config.Access.enforced() {
		return accessGrant{}, nil
	}
	grant := accessGrant{Limits: make(map[string]map[string]int64)}
	request := accessFromContext(r.Context())
	if request == nil {
		return grant, errUnauthenticated()
	}
	for _, ask := range op.Others {
		if !a.accessDoc.published(ask.Action) {
			return grant, errForbidden(ask.Action)
		}
	}
	if !a.accessDoc.published(op.Use.Action) {
		return grant, errForbidden(op.Use.Action)
	}

	// Remove each repeated Other. Remove each Other that equals the use: the same question without usage must never
	// come before the question with usage (ProcurAuth 10.1).
	seen := map[string]bool{askKey(op.Use): true}
	others := make([]accessAsk, 0, len(op.Others))
	for _, ask := range op.Others {
		key := askKey(ask)
		if seen[key] {
			continue
		}
		seen[key] = true
		others = append(others, ask)
	}

	ctx := r.Context()
	if op.Continuation || !a.accessDoc.listsBudget(op.Use.Action) {
		asks := append(append([]accessAsk(nil), others...), op.Use)
		answers := a.pdp.decide(ctx, request.RequestID, request.Subject, questionsOf(asks))
		for index, ask := range asks {
			if err := a.checkAnswer(request, ask, answers[index], grant); err != nil {
				return grant, err
			}
		}
		return grant, nil
	}
	if len(others) > 0 {
		answers := a.pdp.decide(ctx, request.RequestID, request.Subject, questionsOf(others))
		for index, ask := range others {
			if err := a.checkAnswer(request, ask, answers[index], grant); err != nil {
				return grant, err
			}
		}
	}
	answer := a.pdp.use(ctx, request.RequestID, request.Subject, questionsOf([]accessAsk{op.Use})[0])
	if err := a.checkAnswer(request, op.Use, answer, grant); err != nil {
		return grant, err
	}
	return grant, nil
}

// askKey identifies a permission: the action and the resource path.
func askKey(ask accessAsk) string {
	return ask.Action + "\x1f" + ask.Resource.path()
}

func questionsOf(asks []accessAsk) []pdpQuestion {
	questions := make([]pdpQuestion, len(asks))
	for index, ask := range asks {
		questions[index] = pdpQuestion{Action: ask.Action, Resource: ask.Resource}
	}
	return questions
}

// checkAnswer reads one answer. It gives nil for a true answer with valid limits. It adds the ceilings to the grant.
func (a *application) checkAnswer(request *accessRequest, ask accessAsk, answer pdpAnswer, grant accessGrant) error {
	switch {
	case answer.Failed:
		return errPDPUnavailable(ask.Action)
	case !answer.Decision && answer.Reason == pdpReasonBudget && answer.RetryAfter >= 1:
		return errTooManyRequests(ask.Action, answer.RetryAfter)
	case !answer.Decision:
		return errForbidden(ask.Action)
	}
	names := make([]string, 0, len(answer.Limits))
	for name := range answer.Limits {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := answer.Limits[name]
		switch {
		case a.accessDoc.listsCeiling(ask.Action, name):
			if grant.Limits[ask.Action] == nil {
				grant.Limits[ask.Action] = make(map[string]int64)
			}
			if kept, found := grant.Limits[ask.Action][name]; !found || value < kept {
				grant.Limits[ask.Action][name] = value
			}
		case a.accessDoc.isBudget(name):
			// A budget never comes as a limit of an answer (ProcurAuth 7.5).
			log.Printf("access.pdp_invalid_answer request_id=%s action=%s limit=%q", pdpLogToken(request.RequestID), ask.Action, name)
			return errPDPUnavailable(ask.Action)
		default:
			log.Printf("access.ceiling_unenforceable request_id=%s action=%s ceiling=%q", pdpLogToken(request.RequestID), ask.Action, name)
			return errForbidden(ask.Action)
		}
	}
	return nil
}
