package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// The ceiling max_object_size (ProcurAuth 5.5a, 7 and 10.2).
//
// The value of a true answer is the size in bytes of the largest object that one use reads (object.read) or writes
// (object.write). The value applies to the part of the operation that its permission lets run: the value of object.read
// bounds each object that the operation reads, and the value of object.write bounds each object that it writes. A HEAD,
// a list, a tag and a read of metadata read no object bytes: they have no check.
//
// authorize keeps the ceilings in the request. A job keeps the ceilings of its request and gives them to its own context.
// The storage layer (storage_instance_ops.go and the range source) applies them, so each route has the check. A result
// over a ceiling is a refusal. The service never cuts an answer.

const (
	// codeCeilingExceeded is the code of the refusal for a ceiling.
	codeCeilingExceeded = "ceiling_exceeded"
)

// accessCeilings gives the ceilings by action and by name: action, then ceiling name, then value.
type accessCeilings map[string]map[string]int64

// lower keeps the lower value of a ceiling. It gives the map that holds the value.
func (c accessCeilings) lower(action, name string, value int64) accessCeilings {
	if c == nil {
		c = make(accessCeilings)
	}
	if c[action] == nil {
		c[action] = make(map[string]int64)
	}
	if kept, found := c[action][name]; !found || value < kept {
		c[action][name] = value
	}
	return c
}

// clone gives a copy that shares nothing with c. It gives nil when c has no value.
func (c accessCeilings) clone() accessCeilings {
	var copyValue accessCeilings
	for action, names := range c {
		for name, value := range names {
			copyValue = copyValue.lower(action, name, value)
		}
	}
	return copyValue
}

// merge gives the ceilings of c and of other. The lower value of a ceiling wins.
func (c accessCeilings) merge(other accessCeilings) accessCeilings {
	merged := c.clone()
	for action, names := range other {
		for name, value := range names {
			merged = merged.lower(action, name, value)
		}
	}
	return merged
}

// recordCeiling keeps a ceiling of a true answer in the request. The lower value wins. A nil request keeps nothing.
func (r *accessRequest) recordCeiling(action, name string, value int64) {
	if r == nil {
		return
	}
	r.ceilingMu.Lock()
	r.ceilings = r.ceilings.lower(action, name, value)
	r.ceilingMu.Unlock()
}

// ceilingsOfRequest gives a copy of the ceilings of the request.
func (r *accessRequest) ceilingsOfRequest() accessCeilings {
	if r == nil {
		return nil
	}
	r.ceilingMu.Lock()
	defer r.ceilingMu.Unlock()
	return r.ceilings.clone()
}

type accessCeilingsKey struct{}

// withAccessCeilings gives a context that carries the ceilings. A background job uses it: its context has no request.
// The context keeps a copy of the ceilings.
func withAccessCeilings(ctx context.Context, ceilings accessCeilings) context.Context {
	if len(ceilings) == 0 {
		return ctx
	}
	return context.WithValue(ctx, accessCeilingsKey{}, ceilings.clone())
}

// accessCeilingsOf gives a copy of all the ceilings of a context: those of its request and those that
// withAccessCeilings put in it. The lower value wins. It gives nil in mode open.
func accessCeilingsOf(ctx context.Context) accessCeilings {
	if ctx == nil {
		return nil
	}
	var result accessCeilings
	if request := accessFromContext(ctx); request != nil {
		result = request.ceilingsOfRequest()
	}
	if carried, ok := ctx.Value(accessCeilingsKey{}).(accessCeilings); ok {
		result = result.merge(carried)
	}
	return result
}

// accessCeiling gives the ceiling max_object_size of an action for the context. The second result is false when no
// ceiling applies.
func accessCeiling(ctx context.Context, action string) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	var limit int64
	found := false
	if request := accessFromContext(ctx); request != nil {
		request.ceilingMu.Lock()
		if value, ok := request.ceilings[action][limitMaxObjectSize]; ok {
			limit, found = value, true
		}
		request.ceilingMu.Unlock()
	}
	if carried, ok := ctx.Value(accessCeilingsKey{}).(accessCeilings); ok {
		if value, ok := carried[action][limitMaxObjectSize]; ok && (!found || value < limit) {
			limit, found = value, true
		}
	}
	return limit, found
}

// ceilingApplies tells if a ceiling max_object_size applies to one of the actions.
func ceilingApplies(ctx context.Context, actions ...string) bool {
	for _, action := range actions {
		if _, ok := accessCeiling(ctx, action); ok {
			return true
		}
	}
	return false
}

// errCeilingExceeded gives the 403 answer of a refusal for a ceiling. A size below 0 is a size that the service does not
// know. The message never gives the size of the object.
func errCeilingExceeded(action string, limit, size int64) apiError {
	message := "The object is larger than the ceiling max_object_size of the action " + action + "."
	if size < 0 {
		message = "The size of the object is not known. The ceiling max_object_size of the action " + action + " applies."
	}
	return apiError{Status: http.StatusForbidden, Code: codeCeilingExceeded, Message: message, Action: action,
		Ceiling: limitMaxObjectSize, Limit: limit}
}

// checkObjectCeiling checks the size of one object against the ceiling of an action. A size below 0 is an unknown size: a
// ceiling refuses it. It gives nil when no ceiling applies. A refusal writes the log line access.ceiling_refused.
func checkObjectCeiling(ctx context.Context, action string, size int64) error {
	limit, ok := accessCeiling(ctx, action)
	if !ok || (size >= 0 && size <= limit) {
		return nil
	}
	requestID := ""
	if request := accessFromContext(ctx); request != nil {
		requestID = request.RequestID
	}
	log.Printf("access.ceiling_refused request_id=%s action=%s ceiling=%s limit=%d size=%d",
		pdpLogToken(requestID), action, limitMaxObjectSize, limit, size)
	return errCeilingExceeded(action, limit, size)
}

// ceilingErrorOf gives the refusal for a ceiling that an error holds. The second result is false for any other error.
func ceilingErrorOf(err error) (apiError, bool) {
	var refusal apiError
	if errors.As(err, &refusal) && refusal.Code == codeCeilingExceeded {
		return refusal, true
	}
	return apiError{}, false
}

// checkReadCeiling checks the size of an object that the operation reads.
func checkReadCeiling(ctx context.Context, size int64) error {
	return checkObjectCeiling(ctx, actionObjectRead, size)
}

// checkWriteCeiling checks the size of an object that the operation writes.
func checkWriteCeiling(ctx context.Context, size int64) error {
	return checkObjectCeiling(ctx, actionObjectWrite, size)
}

// checkCopyCeilings checks the size of an object that the operation reads and writes: a copy, a move, a restore.
func checkCopyCeilings(ctx context.Context, size int64) error {
	if err := checkReadCeiling(ctx, size); err != nil {
		return err
	}
	return checkWriteCeiling(ctx, size)
}

// readAnswerSize gives the size of the whole object of a read answer: the total of Content-Range, or the Content-Length of
// a full answer. It gives -1 when the answer does not tell the size.
func readAnswerSize(response objectResponse) int64 {
	if contentRange := strings.TrimSpace(response.Header.Get("Content-Range")); contentRange != "" {
		slash := strings.LastIndex(contentRange, "/")
		if slash < 0 {
			return -1
		}
		total, err := strconv.ParseInt(strings.TrimSpace(contentRange[slash+1:]), 10, 64)
		if err != nil || total < 0 {
			return -1
		}
		return total
	}
	if response.StatusCode == http.StatusPartialContent {
		return -1
	}
	length, err := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64)
	if err != nil || length < 0 {
		return -1
	}
	return length
}

// checkReadAnswer applies the ceiling object.read to the answer of a read, before the body goes to the caller. Only an
// answer with object bytes (200 and 206) has the check.
func checkReadAnswer(ctx context.Context, response objectResponse) error {
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return nil
	}
	if !ceilingApplies(ctx, actionObjectRead) {
		return nil
	}
	return checkReadCeiling(ctx, readAnswerSize(response))
}

// copySourceSize gives the size of the source of a copy with a HEAD request. It reads the current version when version is
// empty. It gives -1 when the answer has no size.
func (s *storageInstance) copySourceSize(ctx context.Context, key, version string) (int64, error) {
	response, err := s.HeadVersion(context.WithValue(ctx, objectVersionContextKey{}, ""), key, version)
	if err != nil {
		return -1, err
	}
	closeObjectResponse(response)
	size, err := strconv.ParseInt(strings.TrimSpace(response.Header.Get("Content-Length")), 10, 64)
	if err != nil || size < 0 {
		return -1, nil
	}
	return size, nil
}

// checkCopySource checks a copy of one object. The source size comes from the HEAD request. When no ceiling of read or
// write applies, it makes no request.
func (s *storageInstance) checkCopySource(ctx context.Context, key, version string) error {
	if !ceilingApplies(ctx, actionObjectRead, actionObjectWrite) {
		return nil
	}
	size, err := s.copySourceSize(ctx, key, version)
	if err != nil {
		return err
	}
	return checkCopyCeilings(ctx, size)
}

// checkListedSizes lists each object of a prefix and checks its size before the first effect of an operation on many
// objects. The check uses the ceilings of read and write that apply: a prefix copy reads and writes each object. It
// stops at the first object over a ceiling.
func checkListedSizes(ctx context.Context, instance *storageInstance, prefix string, actions ...string) error {
	if !ceilingApplies(ctx, actions...) {
		return nil
	}
	return forEachObjectAfter(ctx, instance, prefix, "", func(object objectInfo, _ string) error {
		for _, action := range actions {
			if err := checkObjectCeiling(ctx, action, object.Size); err != nil {
				return err
			}
		}
		return nil
	})
}
