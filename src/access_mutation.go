package main

import (
	"errors"
	"net/http"
)

// The permissions of the mutation routes, of the jobs and of the continuations (ProcurAuth 4.5 and 4.7).
//
// A route and the job that it starts build their operation with the same function. The job asks its operation again
// at each read and each control, with Continuation. So the two operations never differ.

// useOperation gives the operation of one permission on the node of a key or of a prefix.
func (a *application) useOperation(action string, instance *storageInstance, rel string) accessOperation {
	return accessOperation{Use: a.objectAsk(action, instance, rel)}
}

// authorizeUse asks one permission on the node of a key or of a prefix. The action lists a budget: the question
// carries usage.
func (a *application) authorizeUse(r *http.Request, action string, instance *storageInstance, rel string) (accessGrant, error) {
	return a.authorize(r, a.useOperation(action, instance, rel))
}

// copyOperation gives the permissions of a copy of a key or of a prefix. source and target are the exact relative keys
// or prefixes that reach the storage.
func (a *application) copyOperation(instance *storageInstance, source, target string) accessOperation {
	return accessOperation{
		Others: []accessAsk{a.objectAsk(actionObjectRead, instance, source)},
		Use:    a.objectAsk(actionObjectWrite, instance, target),
	}
}

// moveOperation gives the permissions of a move of a key or of a prefix. The use is the deletion of the source: it is
// the most destructive action of the move.
func (a *application) moveOperation(instance *storageInstance, source, target string) accessOperation {
	return accessOperation{
		Others: []accessAsk{a.objectAsk(actionObjectRead, instance, source), a.objectAsk(actionObjectWrite, instance, target)},
		Use:    a.objectAsk(actionObjectDelete, instance, source),
	}
}

// deletePrefixOperation gives the permissions of the deletion of a prefix.
func (a *application) deletePrefixOperation(instance *storageInstance, prefix string) accessOperation {
	return accessOperation{
		Others: []accessAsk{a.objectAsk(actionObjectRead, instance, prefix)},
		Use:    a.objectAsk(actionObjectDelete, instance, prefix),
	}
}

// writePrefixOperation gives the permissions of a write of every object under a prefix: the bulk change of tags and the
// abort of the old incomplete uploads.
func (a *application) writePrefixOperation(instance *storageInstance, prefix string) accessOperation {
	return accessOperation{
		Others: []accessAsk{a.objectAsk(actionObjectRead, instance, prefix)},
		Use:    a.objectAsk(actionObjectWrite, instance, prefix),
	}
}

// restoreOperation gives the permissions of the restore of a version of a key.
func (a *application) restoreOperation(instance *storageInstance, key string) accessOperation {
	return accessOperation{
		Others: []accessAsk{a.objectAsk(actionObjectRead, instance, key)},
		Use:    a.objectAsk(actionObjectWrite, instance, key),
	}
}

// lifecycleOperation gives the permissions of the save of the lifecycle rules. The two other permissions are always
// asked, whatever the rules are.
func (a *application) lifecycleOperation(instance *storageInstance) accessOperation {
	return accessOperation{
		Others: []accessAsk{a.bucketAsk(actionObjectRead, instance), a.bucketAsk(actionObjectDelete, instance)},
		Use:    a.bucketAsk(actionBucketAdmin, instance),
	}
}

// abortUploadsOperation gives the permissions of the abort of a list of incomplete uploads. The first key is the use.
// The other keys come first, without usage. keys is not empty.
func (a *application) abortUploadsOperation(instance *storageInstance, keys []string) accessOperation {
	op := accessOperation{Use: a.objectAsk(actionObjectWrite, instance, keys[0])}
	for _, key := range keys[1:] {
		op.Others = append(op.Others, a.objectAsk(actionObjectWrite, instance, key))
	}
	return op
}

// extractOperation gives the permissions of the extraction of entries of an archive. The target instance can differ
// from the source instance.
func (a *application) extractOperation(source *storageInstance, key string, target *storageInstance, targetPrefix string) accessOperation {
	return accessOperation{
		Others: []accessAsk{a.objectAsk(actionObjectRead, source, key), a.objectAsk(actionObjectRead, target, targetPrefix)},
		Use:    a.objectAsk(actionObjectWrite, target, targetPrefix),
	}
}

// jobOperation gives the permissions of the operation that made a job. It gives false for a job that has no rule: the
// caller must refuse such a job. The function reads only the fields that the handler of the route validated.
func (a *application) jobOperation(job jobState) (accessOperation, bool) {
	instance := a.instances[job.Instance]
	if instance == nil {
		return accessOperation{}, false
	}
	switch job.Type {
	case jobTypeCopyPrefix, jobTypeCopyObject:
		return a.copyOperation(instance, job.Source, job.Target), true
	case jobTypeMovePrefix, jobTypeMoveObject:
		return a.moveOperation(instance, job.Source, job.Target), true
	case jobTypeDeletePrefix:
		return a.deletePrefixOperation(instance, job.Prefix), true
	case jobTypeTagPrefix, jobTypeAbortMultiparts:
		return a.writePrefixOperation(instance, job.Prefix), true
	case jobTypeStatsPrefix:
		return a.useOperation(actionObjectRead, instance, job.Prefix), true
	case jobTypeIntegrity:
		if job.IntegrityRequest == nil {
			return accessOperation{}, false
		}
		return a.useOperation(actionObjectRead, instance, job.IntegrityRequest.Key), true
	case jobTypeArchiveEntryIntegrity:
		if job.ArchiveEntryIntegrityRequest == nil {
			return accessOperation{}, false
		}
		return a.useOperation(actionObjectRead, instance, job.ArchiveEntryIntegrityRequest.Key), true
	case jobTypeExtractArchive:
		target := a.instances[job.TargetInstance]
		if target == nil {
			return accessOperation{}, false
		}
		return a.extractOperation(instance, job.Source, target, job.Target), true
	default:
		return accessOperation{}, false
	}
}

// authorizeContinuation asks the permissions of a continuation: the same permissions as the first request, without
// usage.
func (a *application) authorizeContinuation(r *http.Request, op accessOperation) error {
	op.Continuation = true
	_, err := a.authorize(r, op)
	return err
}

// isForbiddenError tells if an error is the refusal 403 of the PDP.
func isForbiddenError(err error) bool {
	var apiErr apiError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden && apiErr.Code == codeForbidden
}

// cleanObjectPair cleans the source and the target of a copy or a move of one object. It refuses an empty key and two
// equal keys, as copyOrMove does. It makes no storage call.
func cleanObjectPair(source, target string) (string, string, error) {
	source, target = cleanRelativeKey(source), cleanRelativeKey(target)
	if source == "" || target == "" {
		return "", "", apiError{Status: http.StatusBadRequest, Code: "invalid_path", Message: "src and dst are required"}
	}
	if source == target {
		return "", "", apiError{Status: http.StatusBadRequest, Code: "same_path", Message: "src and dst must be different"}
	}
	return source, target, nil
}

// uploadAuthorizer gives the check of an upload request: it asks object.write on the node of the key, and it takes the
// instance and the key from the upload, never from the client. A continuation asks no usage.
func (a *application) uploadAuthorizer(r *http.Request, continuation bool) func(*storageInstance, string) error {
	return func(instance *storageInstance, key string) error {
		op := a.useOperation(actionObjectWrite, instance, key)
		op.Continuation = continuation
		_, err := a.authorize(r, op)
		return err
	}
}
