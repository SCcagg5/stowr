package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"
)

// The action document of the service (ProcurAuth 5.1 to 5.5a). The service builds the document one time at start, from
// the typed configuration, and it serves the same bytes on each read. In mode "open" the service builds no document.

const (
	actionObjectRead   = "object.read"
	actionObjectWrite  = "object.write"
	actionObjectDelete = "object.delete"
	actionObjectLock   = "object.lock"
	actionBucketAdmin  = "bucket.admin"

	limitWrites        = "writes"
	limitMaxObjectSize = "max_object_size"

	accessActionsPath = "/.well-known/procurauth/actions"

	// maxAccessDocumentBytes is the size that each reader accepts (ProcurAuth 5.3c).
	maxAccessDocumentBytes = 64 << 10
	// maxAccessLabelRunes is the longest label that each reader accepts (ProcurAuth 5.3c).
	maxAccessLabelRunes = 60
	// maxObjectSizeCeiling is the largest object of the S3 protocol: 5 TiB.
	maxObjectSizeCeiling = int64(5497558138880)
)

// documentType is one resource type of the document.
type documentType struct {
	Name        string `json:"name"`
	Parent      string `json:"parent"`
	Description string `json:"description"`
}

// documentAction is one action of the document.
type documentAction struct {
	Name          string   `json:"name"`
	ResourceTypes []string `json:"resource_types"`
	Description   string   `json:"description"`
	Limits        []string `json:"limits,omitempty"`
}

// documentResource is one node that the service names. It has no actions member: it takes each action that lists
// its type (ProcurAuth 5.2).
type documentResource struct {
	Path  string `json:"path"`
	Label string `json:"label,omitempty"`
}

// documentLimit is one limit declaration. A budget has no min and no max. A ceiling has both.
type documentLimit struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Unit        string `json:"unit"`
	Kind        string `json:"kind"`
	Min         *int64 `json:"min,omitempty"`
	Max         *int64 `json:"max,omitempty"`
}

// documentBody is the JSON object of the document. The order of the members is the order of the fields.
type documentBody struct {
	Server        string             `json:"server"`
	ResourceTypes []documentType     `json:"resource_types"`
	Actions       []documentAction   `json:"actions"`
	Resources     []documentResource `json:"resources,omitempty"`
	Limits        []documentLimit    `json:"limits,omitempty"`
}

// accessDocument is the action document, built one time. The zero value is the document of mode "open": it publishes
// nothing.
type accessDocument struct {
	raw     []byte
	actions map[string]documentAction
	limits  map[string]documentLimit
}

// accessActionRow is one row of the table of the actions: the action, its text, its limits, and the rule of the
// configuration that decides if the service serves it.
type accessActionRow struct {
	name        string
	types       []string
	description string
	limits      []string
}

var accessAllTypes = []string{accessTypeService, accessTypeBucket, accessTypeFolder, accessTypeSubfolder}

// accessActionRows are the actions in the order of the document.
var accessActionRows = []accessActionRow{
	{actionObjectRead, accessAllTypes,
		"List, preview, inspect and download objects. Show versions, tags, metadata, usage and archive entries. Check the integrity of an object.",
		[]string{limitMaxObjectSize}},
	{actionObjectWrite, accessAllTypes,
		"Upload objects, make folders, restore versions, and change tags and metadata. Be the destination of a copy, a move or an extraction.",
		[]string{limitWrites, limitMaxObjectSize}},
	{actionObjectDelete, accessAllTypes,
		"Delete objects, versions and folders. Be the source of a move or a rename.",
		[]string{limitWrites}},
	{actionObjectLock, accessAllTypes,
		"Set or remove a legal hold. Set, extend or shorten the retention of an object version.",
		[]string{limitWrites}},
	{actionBucketAdmin, []string{accessTypeService, accessTypeBucket},
		"Change the lifecycle rules of a bucket.",
		[]string{limitWrites}},
}

// accessResourceTypes are the types below the service, from the top.
var accessResourceTypes = []documentType{
	{accessTypeBucket, accessTypeService, "One bucket block or one volume block of the configuration. The id is the label of the block."},
	{accessTypeFolder, accessTypeBucket, "One top-level folder of a bucket or a volume, below its root prefix. The id is the first part of the object key."},
	{accessTypeSubfolder, accessTypeFolder, "One folder in a top-level folder. The id is the second part of the object key."},
}

// accessLimitDeclarations are the limits that the service can declare, in the order of the document.
var accessLimitDeclarations = func() []documentLimit {
	minSize, maxSize := int64(1), maxObjectSizeCeiling
	return []documentLimit{
		{Name: limitWrites, Label: "Writes", Description: "Operations that change objects or bucket settings.", Unit: "operation", Kind: "budget"},
		{Name: limitMaxObjectSize, Label: "Largest object", Description: "The largest object that one use reads or writes, in bytes.",
			Unit: "byte", Kind: "ceiling", Min: &minSize, Max: &maxSize},
	}
}()

// instanceAllows tells if the permission list of a block gives a permission. A block without a list gives read, write
// and delete. This is the process ceiling of storage.go, without the provider probes.
func instanceAllows(permissions []string, defined bool, permission string) bool {
	return !defined || containsString(permissions, permission)
}

// buildAccessDocument builds the action document from the configuration. In mode "open" it builds nothing. In mode
// "authzen" it gives an error when the JSON is larger than 64 KiB (ProcurAuth 5.3c).
func buildAccessDocument(cfg appConfig) (accessDocument, error) {
	if !cfg.Access.enforced() {
		return accessDocument{}, nil
	}
	readOnly := cfg.Runtime.forceReadOnly()
	// bucketReadWrite: one bucket block gives read and write. The handlers of Object Lock and of the lifecycle rules need
	// both, and a volume has neither feature.
	var read, write, del, bucketReadWrite bool
	for _, bucket := range cfg.Buckets {
		r := instanceAllows(bucket.Permissions, bucket.PermissionsDefined, permissionRead)
		w := instanceAllows(bucket.Permissions, bucket.PermissionsDefined, permissionWrite)
		read = read || r
		write = write || w
		bucketReadWrite = bucketReadWrite || (r && w)
		del = del || instanceAllows(bucket.Permissions, bucket.PermissionsDefined, permissionDelete)
	}
	for _, volume := range cfg.Volumes {
		read = read || instanceAllows(volume.Permissions, volume.PermissionsDefined, permissionRead)
		write = write || instanceAllows(volume.Permissions, volume.PermissionsDefined, permissionWrite)
		del = del || instanceAllows(volume.Permissions, volume.PermissionsDefined, permissionDelete)
	}
	serves := map[string]bool{
		actionObjectRead:   read,
		actionObjectWrite:  !readOnly && write,
		actionObjectDelete: !readOnly && del,
	}
	serves[actionObjectLock] = serves[actionObjectWrite] && bucketReadWrite
	serves[actionBucketAdmin] = serves[actionObjectWrite] && bucketReadWrite

	doc := accessDocument{actions: make(map[string]documentAction), limits: make(map[string]documentLimit)}
	body := documentBody{Server: cfg.Access.Audience, ResourceTypes: accessResourceTypes, Actions: []documentAction{}}
	listed := make(map[string]bool)
	for _, row := range accessActionRows {
		if !serves[row.name] {
			continue
		}
		action := documentAction{Name: row.name, ResourceTypes: row.types, Description: row.description, Limits: row.limits}
		body.Actions = append(body.Actions, action)
		doc.actions[row.name] = action
		for _, limit := range row.limits {
			listed[limit] = true
		}
	}
	for _, limit := range accessLimitDeclarations {
		if listed[limit.Name] {
			body.Limits = append(body.Limits, limit)
			doc.limits[limit.Name] = limit
		}
	}
	for _, bucket := range cfg.Buckets {
		body.Resources = append(body.Resources, accessResourceItem(bucket.ID, bucket.Name))
	}
	for _, volume := range cfg.Volumes {
		body.Resources = append(body.Resources, accessResourceItem(volume.ID, volume.Name))
	}

	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		return accessDocument{}, fmt.Errorf("encode the action document: %w", err)
	}
	doc.raw = bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))
	if len(doc.raw) > maxAccessDocumentBytes {
		return accessDocument{}, fmt.Errorf("the action document has %d bytes, and the limit is %d bytes (ProcurAuth 5.3c): use fewer bucket blocks and volume blocks, or shorter labels and names",
			len(doc.raw), maxAccessDocumentBytes)
	}
	return doc, nil
}

// accessResourceItem gives the resource of a bucket block or of a volume block. The label is the name, only when the
// name has 1 to 60 characters and no control character.
func accessResourceItem(id, name string) documentResource {
	item := documentResource{Path: accessTypeBucket + ":" + percentEncodeID(id)}
	if utf8.ValidString(name) && !hasControlCharacter(name) {
		if count := utf8.RuneCountInString(name); count >= 1 && count <= maxAccessLabelRunes {
			item.Label = name
		}
	}
	return item
}

// json gives the bytes of the document. The bytes are empty in mode "open".
func (d accessDocument) json() []byte { return d.raw }

// published tells if the document has the action.
func (d accessDocument) published(action string) bool {
	_, ok := d.actions[action]
	return ok
}

// listsBudget tells if the published action lists a budget.
func (d accessDocument) listsBudget(action string) bool {
	for _, name := range d.actions[action].Limits {
		if d.isBudget(name) {
			return true
		}
	}
	return false
}

// listsCeiling tells if the published action lists the ceiling.
func (d accessDocument) listsCeiling(action, ceiling string) bool {
	limit, ok := d.limits[ceiling]
	if !ok || limit.Kind != "ceiling" {
		return false
	}
	for _, name := range d.actions[action].Limits {
		if name == ceiling {
			return true
		}
	}
	return false
}

// isBudget tells if the document declares the limit and the limit is a budget.
func (d accessDocument) isBudget(name string) bool {
	return d.limits[name].Kind == "budget"
}

// serveAccessActions answers a read of the action document. It needs no identity token. It never asks the PDP. In
// mode "open" the route does not exist.
func serveAccessActions(w http.ResponseWriter, r *http.Request, cfg accessConfig, doc accessDocument) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !cfg.enforced() {
		http.NotFound(w, r)
		return
	}
	body := doc.json()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(body)
	}
}

// The coordinator adds this method after the field accessDoc exists in the application struct:
//
//	func (a *application) handleAccessActions(w http.ResponseWriter, r *http.Request) {
//		serveAccessActions(w, r, a.config.Access, a.accessDoc)
//	}
