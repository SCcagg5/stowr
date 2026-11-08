package main

import (
	"net/http"
)

// The display questions of the access policy (ProcurAuth 4.4). They decide what the interface shows: the list of the
// buckets, the controls of a folder and the path to a folder that the person can read. They never decide an
// operation. Each route still asks its own question before it acts. A match "subtree" never lets an operation run.

// displayActions are the actions of the display questions on a bucket node, in the order of the questions.
var displayActions = []string{actionObjectRead, actionObjectWrite, actionObjectDelete, actionObjectLock, actionBucketAdmin}

// listAccessJSON gives the decisions at the node of a listing (match "node"). The interface uses them to hide the
// controls that the person cannot use.
type listAccessJSON struct {
	Read   bool `json:"read"`
	Write  bool `json:"write"`
	Delete bool `json:"delete"`
}

// listResponseWithAccess is the answer of GET /api/list in mode authzen. In mode open the access member is absent.
type listResponseWithAccess struct {
	listResponseJSON
	Access *listAccessJSON `json:"access,omitempty"`
}

// listDisplay is the result of the display questions of one listing. Access is nil in mode open.
type listDisplay struct {
	Access *listAccessJSON
	// Navigation is true when the person cannot read the node, but can read a folder below it. The listing then
	// shows the folders only.
	Navigation bool
}

// displayRequest gives the identity of the request, or the 401 error when the middleware did not set it.
func displayRequest(r *http.Request) (*accessRequest, error) {
	request := accessFromContext(r.Context())
	if request == nil {
		return nil, errUnauthenticated()
	}
	return request, nil
}

// visibleInstances gives the instances that the answer of GET /api/instances shows, and the default instance. In mode
// open it gives all instances. In mode authzen it asks one batch of display questions (match "subtree", no usage) on
// the bucket node of each instance. An instance is visible when one answer is true. A failed answer gives a 503
// error and never an empty list.
func (a *application) visibleInstances(r *http.Request) ([]instanceInfo, string, error) {
	infos := make([]instanceInfo, 0, len(a.order))
	defaultID := ""
	if !a.config.Access.enforced() {
		for _, id := range a.order {
			instance := a.instances[id]
			infos = append(infos, instance.publicInfo())
			instance.refreshBucketFacts()
		}
		if len(a.order) > 0 {
			defaultID = a.order[0]
		}
		return infos, defaultID, nil
	}

	request, err := displayRequest(r)
	if err != nil {
		return nil, "", err
	}
	type slot struct {
		instance *storageInstance
		action   string
	}
	var slots []slot
	var questions []pdpQuestion
	for _, id := range a.order {
		instance := a.instances[id]
		for _, action := range displayActions {
			if !a.accessDoc.published(action) {
				continue
			}
			slots = append(slots, slot{instance: instance, action: action})
			questions = append(questions, pdpQuestion{
				Action:   action,
				Resource: bucketResource(a.config.Access.Audience, instance.cfg.ID),
				Subtree:  true,
			})
		}
	}
	answers := a.pdp.decide(r.Context(), request.RequestID, request.Subject, questions)
	held := make(map[string]map[string]bool, len(a.order))
	for index, answer := range answers {
		if answer.Failed {
			return nil, "", errPDPUnavailable(slots[index].action)
		}
		id := slots[index].instance.cfg.ID
		if held[id] == nil {
			held[id] = make(map[string]bool, len(displayActions))
		}
		if answer.Decision {
			held[id][slots[index].action] = true
		}
	}
	for _, id := range a.order {
		if len(held[id]) == 0 {
			continue
		}
		instance := a.instances[id]
		infos = append(infos, withAccessCapabilities(instance.publicInfo(), held[id]))
		instance.refreshBucketFacts()
		if defaultID == "" {
			defaultID = id
		}
	}
	return infos, defaultID, nil
}

// withAccessCapabilities marks as denied, with the source "access", each capability whose action the person holds
// nowhere in the bucket. Then it computes the operations again from the capabilities. This is display only: the
// server still checks each operation.
//
// The fields of Object Lock and of the lifecycle rules of the instance tell what the bucket supports. They are not
// capabilities of the person, so this function does not change them.
func withAccessCapabilities(info instanceInfo, held map[string]bool) instanceInfo {
	for action, permission := range map[string]string{
		actionObjectRead:   permissionRead,
		actionObjectWrite:  permissionWrite,
		actionObjectDelete: permissionDelete,
	} {
		if !held[action] {
			info.Capabilities.set(permission, deniedCapability("access", "the access policy does not give "+action+" in this bucket", true))
		}
	}
	info.Operations = operationsOf(info.Capabilities)
	return info
}

// operationsOf computes the operations from the three capabilities. The rules are the rules of
// storageInstance.operationCapabilities.
func operationsOf(caps capabilities) map[string]capabilityState {
	read, write, remove := caps.Read, caps.Write, caps.Delete
	return map[string]capabilityState{
		"list": read, "preview": read, "download": read,
		"upload": write, "overwrite": write, "createFolder": write,
		"copy":   combineCapabilityStates(read, write),
		"move":   combineCapabilityStates(read, write, remove),
		"rename": combineCapabilityStates(read, write, remove),
		"delete": remove,
	}
}

// hasDeeperType tells if a node of this type has a node type below it in the tree.
func hasDeeperType(nodeType string) bool {
	return nodeType == accessTypeBucket || nodeType == accessTypeFolder
}

// listDisplay asks one batch of display questions at the node of the listed prefix (match "node"): object.read and,
// when the document publishes them, object.write and object.delete. In mode open it asks nothing and gives no access
// member. A failed answer gives a 503 error. When the person cannot read the node, the answer depends on the node:
// a bucket or a folder gives a navigation listing, and a subfolder gives the 403 error.
func (a *application) listDisplay(r *http.Request, instance *storageInstance, prefix string) (listDisplay, error) {
	if !a.config.Access.enforced() {
		return listDisplay{}, nil
	}
	request, err := displayRequest(r)
	if err != nil {
		return listDisplay{}, err
	}
	if !a.accessDoc.published(actionObjectRead) {
		return listDisplay{}, errForbidden(actionObjectRead)
	}
	actions := []string{actionObjectRead}
	for _, action := range []string{actionObjectWrite, actionObjectDelete} {
		if a.accessDoc.published(action) {
			actions = append(actions, action)
		}
	}
	resource := a.objectAsk(actionObjectRead, instance, prefix).Resource
	questions := make([]pdpQuestion, len(actions))
	for index, action := range actions {
		questions[index] = pdpQuestion{Action: action, Resource: resource}
	}
	answers := a.pdp.decide(r.Context(), request.RequestID, request.Subject, questions)
	access := &listAccessJSON{}
	for index, answer := range answers {
		if answer.Failed {
			return listDisplay{}, errPDPUnavailable(actions[index])
		}
		switch actions[index] {
		case actionObjectRead:
			access.Read = answer.Decision
			if answer.Decision {
				// The ceilings of a true answer must be valid, as for each other read.
				if err := a.checkAnswer(request, accessAsk{Action: actionObjectRead, Resource: resource}, answer, accessGrant{Limits: map[string]map[string]int64{}}); err != nil {
					return listDisplay{}, err
				}
			}
		case actionObjectWrite:
			access.Write = answer.Decision
		case actionObjectDelete:
			access.Delete = answer.Decision
		}
	}
	if access.Read {
		return listDisplay{Access: access}, nil
	}
	if !hasDeeperType(resource.Type) {
		return listDisplay{}, errForbidden(actionObjectRead)
	}
	return listDisplay{Access: access, Navigation: true}, nil
}

// navigationItems keeps the folder items that the person can read (object.read, match "subtree", at the node of the
// folder) and removes each object item. It asks one batch with one question for each distinct node. A failed answer
// gives a 503 error. The listing that it gives shows the path to a folder. It lets no operation run.
func (a *application) navigationItems(r *http.Request, instance *storageInstance, items []listItemJSON) ([]listItemJSON, error) {
	request, err := displayRequest(r)
	if err != nil {
		return nil, err
	}
	questions := make([]pdpQuestion, 0, len(items))
	index := make(map[string]int, len(items))
	nodeOf := make([]int, len(items))
	for position, item := range items {
		nodeOf[position] = -1
		if item.Type != "prefix" {
			continue
		}
		resource := a.objectAsk(actionObjectRead, instance, item.Prefix).Resource
		path := resource.path()
		found, known := index[path]
		if !known {
			found = len(questions)
			index[path] = found
			questions = append(questions, pdpQuestion{Action: actionObjectRead, Resource: resource, Subtree: true})
		}
		nodeOf[position] = found
	}
	kept := make([]listItemJSON, 0, len(items))
	if len(questions) == 0 {
		return kept, nil
	}
	answers := a.pdp.decide(r.Context(), request.RequestID, request.Subject, questions)
	for _, answer := range answers {
		if answer.Failed {
			return nil, errPDPUnavailable(actionObjectRead)
		}
	}
	for position, item := range items {
		if nodeOf[position] >= 0 && answers[nodeOf[position]].Decision {
			kept = append(kept, item)
		}
	}
	return kept, nil
}
