package main

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// The resource tree of the access policy (docs/ACCESS.md, "Resource tree").
//
//	service (access.identity.audience)
//	└── bucket (the label of a bucket block or of a volume block)
//	    └── folder (the first part of the object key, below root_prefix)
//	        └── subfolder (the second part of the object key)
//
// An object is not a node. The node of an object key or of a listing prefix is the deepest folder that holds all the
// keys of the operation. The PEP sends this node and the chain of its ancestors to the PDP (ProcurAuth 4.1).

const (
	accessTypeService   = "service"
	accessTypeBucket    = "bucket"
	accessTypeFolder    = "folder"
	accessTypeSubfolder = "subfolder"

	// maxAccessIDBytes is the longest id of a node (ProcurAuth 4.1).
	maxAccessIDBytes = 256
)

// accessFolderTypes are the types below a bucket, from the top. The length of this list is the number of folder
// levels of the tree.
var accessFolderTypes = []string{accessTypeFolder, accessTypeSubfolder}

// accessNode is one node of the resource tree: its type and its id.
type accessNode struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// accessResource is the resource of a question to the PDP: the node and the chain of its ancestors, from the service
// to the parent of the node. The service has no ancestors.
type accessResource struct {
	Type      string
	ID        string
	Ancestors []accessNode
}

// path gives the text form of the chain, for example "service:infra-s3/bucket:logs/folder:2026". Each id is
// percent-encoded (ProcurAuth 5.2). The logs and the keys of the answer cache use this form.
func (r accessResource) path() string {
	var b strings.Builder
	for _, node := range r.Ancestors {
		b.WriteString(node.Type)
		b.WriteByte(':')
		b.WriteString(percentEncodeID(node.ID))
		b.WriteByte('/')
	}
	b.WriteString(r.Type)
	b.WriteByte(':')
	b.WriteString(percentEncodeID(r.ID))
	return b.String()
}

// MarshalJSON gives the AuthZEN resource. A node below the service carries its chain in properties.ancestors. The
// service carries no properties.
func (r accessResource) MarshalJSON() ([]byte, error) {
	type properties struct {
		Ancestors []accessNode `json:"ancestors"`
	}
	type resource struct {
		Type       string      `json:"type"`
		ID         string      `json:"id"`
		Properties *properties `json:"properties,omitempty"`
	}
	out := resource{Type: r.Type, ID: r.ID}
	if len(r.Ancestors) > 0 {
		out.Properties = &properties{Ancestors: r.Ancestors}
	}
	return json.Marshal(out)
}

// percentEncodeID encodes each byte outside A-Z, a-z, 0-9, '-', '.', '_' and '~' as %XX with upper-case hexadecimal
// digits (ProcurAuth 5.2).
func percentEncodeID(id string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// validAccessID tells if a value can be the id of a node: 1 to 256 bytes of UTF-8 without a control character
// (ProcurAuth 4.1).
func validAccessID(value string) bool {
	return value != "" && len(value) <= maxAccessIDBytes && utf8.ValidString(value) && !hasControlCharacter(value)
}

// serviceResource gives the node of the service. It has no ancestors.
func serviceResource(service string) accessResource {
	return accessResource{Type: accessTypeService, ID: service}
}

// bucketResource gives the node of a bucket block or of a volume block.
func bucketResource(service, bucket string) accessResource {
	return accessResource{Type: accessTypeBucket, ID: bucket, Ancestors: []accessNode{{Type: accessTypeService, ID: service}}}
}

// keyResource gives the node of a key of a bucket. rel is the exact key or prefix that the route sends to the
// storage, relative to root_prefix. The function cuts rel at each "/" and removes the last part: the name of an
// object, the empty part after the "/" at the end of a folder path, or the incomplete last part of a listing prefix.
// The remaining parts give the folder levels, from the top. The walk stops at the depth of the tree, and at the first
// part that cannot be an id (empty, longer than 256 bytes, not UTF-8, or with a control character). A walk that stops
// early gives a node nearer to the bucket: a grant must then be broader, never narrower.
//
// Examples, for the bucket b: "" and "a.txt" give bucket:b. "reports/q1.csv" and "reports/" give folder:reports.
// "reports/2026/q1.csv" and "reports/2026/q1/x.csv" give subfolder:2026 below folder:reports. The prefix "rep" gives
// bucket:b, because it matches keys in several folders.
func keyResource(service, bucket, rel string) accessResource {
	chain := []accessNode{{Type: accessTypeService, ID: service}, {Type: accessTypeBucket, ID: bucket}}
	parts := strings.Split(rel, "/")
	parts = parts[:len(parts)-1]
	for level, part := range parts {
		if level == len(accessFolderTypes) || !validAccessID(part) {
			break
		}
		chain = append(chain, accessNode{Type: accessFolderTypes[level], ID: part})
	}
	last := chain[len(chain)-1]
	return accessResource{Type: last.Type, ID: last.ID, Ancestors: chain[:len(chain)-1]}
}
