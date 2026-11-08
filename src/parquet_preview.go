package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	maxParquetMetadataBytes = int64(128 << 20)
	maxParquetSchemaFields  = 20_000
	maxParquetRowGroups     = 10_000
)

type parquetSchemaField struct {
	Path         string `json:"path"`
	Name         string `json:"name"`
	PhysicalType string `json:"physicalType,omitempty"`
	LogicalType  string `json:"logicalType,omitempty"`
	Repetition   string `json:"repetition,omitempty"`
	Children     int    `json:"children,omitempty"`
	TypeLength   int64  `json:"typeLength,omitempty"`
	Precision    int64  `json:"precision,omitempty"`
	Scale        int64  `json:"scale,omitempty"`
}

type parquetRowGroupInfo struct {
	Index             int   `json:"index"`
	Rows              int64 `json:"rows"`
	Columns           int   `json:"columns"`
	UncompressedBytes int64 `json:"uncompressedBytes,omitempty"`
	CompressedBytes   int64 `json:"compressedBytes,omitempty"`
}

type parquetPreviewResponse struct {
	Instance        string                `json:"instance"`
	Key             string                `json:"key"`
	Version         int64                 `json:"version"`
	Rows            int64                 `json:"rows"`
	Columns         int                   `json:"columns"`
	CreatedBy       string                `json:"createdBy,omitempty"`
	Schema          []parquetSchemaField  `json:"schema"`
	RowGroups       []parquetRowGroupInfo `json:"rowGroups"`
	RowGroupCount   int                   `json:"rowGroupCount"`
	SchemaTruncated bool                  `json:"schemaTruncated,omitempty"`
	GroupsTruncated bool                  `json:"rowGroupsTruncated,omitempty"`
	MetadataBytes   int64                 `json:"metadataBytes"`
	StorageBytes    int64                 `json:"storageBytes"`
	StorageRequests int                   `json:"storageRequests"`
	DataRowsDecoded bool                  `json:"dataRowsDecoded"`
}

type parquetSchemaElementDetail struct {
	Name             string
	PhysicalType     int64
	HasPhysicalType  bool
	TypeLength       int64
	Repetition       int64
	HasRepetition    bool
	Children         int
	ConvertedType    int64
	HasConvertedType bool
	Scale            int64
	Precision        int64
}

type parquetMetadataDetail struct {
	Version    int64
	Rows       int64
	CreatedBy  string
	Schema     []parquetSchemaElementDetail
	RowGroups  []parquetRowGroupInfo
	GroupCount int
}

func (a *application) handleParquetPreview(w http.ResponseWriter, r *http.Request) {
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
	key := strings.TrimLeft(r.URL.Query().Get("key"), "/")
	if key == "" {
		writeAPIError(w, apiError{Status: http.StatusBadRequest, Code: "missing_key", Message: "key is required"})
		return
	}
	if _, err := a.authorizeRead(r, instance, key); err != nil {
		writeAPIError(w, err)
		return
	}
	knownSize, err := parseOptionalObjectSize(r.URL.Query().Get("size"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	response, err := readParquetPreview(r.Context(), instance, key, knownSize, r.URL.Query().Get("etag"))
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func readParquetPreview(ctx context.Context, instance *storageInstance, key string, knownSize int64, expectedETag string) (parquetPreviewResponse, error) {
	response := parquetPreviewResponse{Instance: instance.cfg.ID, Key: key, DataRowsDecoded: false}
	source, err := openObjectRangeSource(ctx, instance, key)
	if err != nil {
		return response, err
	}
	source.SetKnownSize(knownSize)
	source.SetExpectedETag(expectedETag)
	footer, err := source.ReadSuffix(8)
	if err != nil {
		return response, err
	}
	if len(footer) != 8 || string(footer[4:]) != "PAR1" {
		return response, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_parquet", Message: "the object is not a valid Parquet file"}
	}
	metadataLength := int64(binary.LittleEndian.Uint32(footer[:4]))
	if metadataLength <= 0 || metadataLength > maxParquetMetadataBytes || source.Size() < metadataLength+8 {
		return response, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_parquet_metadata", Message: "the Parquet footer metadata length is invalid or exceeds the preview limit"}
	}
	metadata, err := source.ReadRange(source.Size()-8-metadataLength, metadataLength)
	if err != nil {
		return response, err
	}
	detail, err := parseParquetMetadataDetail(metadata)
	if err != nil {
		return response, apiError{Status: http.StatusUnprocessableEntity, Code: "invalid_parquet_metadata", Message: err.Error()}
	}
	fields, truncated := flattenParquetSchema(detail.Schema)
	response.Version = detail.Version
	response.Rows = detail.Rows
	response.Schema = fields
	response.Columns = parquetTopLevelColumns(detail.Schema)
	response.CreatedBy = detail.CreatedBy
	response.RowGroups = detail.RowGroups
	response.RowGroupCount = detail.GroupCount
	response.SchemaTruncated = truncated
	response.GroupsTruncated = detail.GroupCount > len(detail.RowGroups)
	response.MetadataBytes = metadataLength
	response.StorageBytes = source.BytesRead()
	response.StorageRequests = source.RequestCount()
	return response, nil
}

func parseParquetMetadataDetail(data []byte) (parquetMetadataDetail, error) {
	reader := &thriftCompactReader{data: data}
	var detail parquetMetadataDetail
	lastField := int16(0)
	for {
		fieldID, fieldType, nextLast, err := reader.fieldHeader(lastField)
		if err != nil {
			return detail, err
		}
		if fieldType == compactStop {
			break
		}
		lastField = nextLast
		switch fieldID {
		case 1:
			value, err := reader.readCompactInteger(fieldType)
			if err != nil {
				return detail, err
			}
			detail.Version = value
		case 2:
			if fieldType != compactList {
				return detail, fmt.Errorf("unexpected Parquet schema field type")
			}
			size, elementType, err := reader.collectionHeader()
			if err != nil {
				return detail, err
			}
			if elementType != compactStruct {
				return detail, fmt.Errorf("unexpected Parquet schema element type")
			}
			if size > maxParquetSchemaFields*2 {
				return detail, fmt.Errorf("Parquet schema contains too many fields")
			}
			detail.Schema = make([]parquetSchemaElementDetail, 0, minInt(size, maxParquetSchemaFields+1))
			for index := 0; index < size; index++ {
				element, err := reader.readParquetSchemaElementDetail()
				if err != nil {
					return detail, err
				}
				if len(detail.Schema) < maxParquetSchemaFields+1 {
					detail.Schema = append(detail.Schema, element)
				}
			}
		case 3:
			value, err := reader.readCompactInteger(fieldType)
			if err != nil {
				return detail, err
			}
			detail.Rows = value
		case 4:
			if fieldType != compactList {
				return detail, fmt.Errorf("unexpected Parquet row-group field type")
			}
			size, elementType, err := reader.collectionHeader()
			if err != nil {
				return detail, err
			}
			detail.GroupCount = size
			// Every element takes at least one byte, so a larger count is corrupt.
			if size > len(reader.data)-reader.pos {
				return detail, io.ErrUnexpectedEOF
			}
			for index := 0; index < size; index++ {
				if elementType != compactStruct {
					if err := reader.skipValue(elementType, true); err != nil {
						return detail, err
					}
					continue
				}
				group, err := reader.readParquetRowGroup(index)
				if err != nil {
					return detail, err
				}
				if len(detail.RowGroups) < maxParquetRowGroups {
					detail.RowGroups = append(detail.RowGroups, group)
				}
			}
		case 6:
			if fieldType != compactBinary {
				if err := reader.skip(fieldType); err != nil {
					return detail, err
				}
				continue
			}
			value, err := reader.readCompactBinary(4096)
			if err != nil {
				return detail, err
			}
			detail.CreatedBy = value
		default:
			if err := reader.skip(fieldType); err != nil {
				return detail, err
			}
		}
	}
	if detail.Rows < 0 {
		return parquetMetadataDetail{}, fmt.Errorf("Parquet metadata contains an invalid row count")
	}
	return detail, nil
}

func (r *thriftCompactReader) readCompactInteger(fieldType byte) (int64, error) {
	if fieldType != compactI16 && fieldType != compactI32 && fieldType != compactI64 {
		return 0, fmt.Errorf("unexpected Thrift integer type %d", fieldType)
	}
	return r.readZigZag()
}

func (r *thriftCompactReader) readCompactBinary(maxLength int) (string, error) {
	length, err := r.readVarint()
	if err != nil {
		return "", err
	}
	if length > uint64(len(r.data)-r.pos) {
		return "", io.ErrUnexpectedEOF
	}
	if length > uint64(maxLength) {
		if err := r.advance(int(length)); err != nil {
			return "", err
		}
		return "", nil
	}
	value := string(r.data[r.pos : r.pos+int(length)])
	r.pos += int(length)
	return value, nil
}

func (r *thriftCompactReader) readParquetSchemaElementDetail() (parquetSchemaElementDetail, error) {
	var element parquetSchemaElementDetail
	lastField := int16(0)
	for {
		fieldID, fieldType, nextLast, err := r.fieldHeader(lastField)
		if err != nil {
			return element, err
		}
		if fieldType == compactStop {
			return element, nil
		}
		lastField = nextLast
		switch fieldID {
		case 1:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return element, err
			}
			element.PhysicalType, element.HasPhysicalType = value, true
		case 2:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return element, err
			}
			element.TypeLength = value
		case 3:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return element, err
			}
			element.Repetition, element.HasRepetition = value, true
		case 4:
			if fieldType != compactBinary {
				return element, fmt.Errorf("unexpected Parquet schema name type")
			}
			value, err := r.readCompactBinary(16 << 10)
			if err != nil {
				return element, err
			}
			element.Name = value
		case 5:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return element, err
			}
			if value < 0 || value > maxParquetSchemaFields {
				return element, fmt.Errorf("Parquet schema contains an invalid child count")
			}
			element.Children = int(value)
		case 6:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return element, err
			}
			element.ConvertedType, element.HasConvertedType = value, true
		case 7:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return element, err
			}
			element.Scale = value
		case 8:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return element, err
			}
			element.Precision = value
		default:
			if err := r.skip(fieldType); err != nil {
				return element, err
			}
		}
	}
}

func (r *thriftCompactReader) readParquetRowGroup(index int) (parquetRowGroupInfo, error) {
	group := parquetRowGroupInfo{Index: index}
	lastField := int16(0)
	for {
		fieldID, fieldType, nextLast, err := r.fieldHeader(lastField)
		if err != nil {
			return group, err
		}
		if fieldType == compactStop {
			return group, nil
		}
		lastField = nextLast
		switch fieldID {
		case 1:
			if fieldType != compactList {
				return group, fmt.Errorf("unexpected Parquet column-chunk list type")
			}
			size, elementType, err := r.collectionHeader()
			if err != nil {
				return group, err
			}
			group.Columns = size
			if size > len(r.data)-r.pos {
				return group, io.ErrUnexpectedEOF
			}
			for item := 0; item < size; item++ {
				if err := r.skipValue(elementType, true); err != nil {
					return group, err
				}
			}
		case 2:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return group, err
			}
			group.UncompressedBytes = value
		case 3:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return group, err
			}
			group.Rows = value
		case 6:
			value, err := r.readCompactInteger(fieldType)
			if err != nil {
				return group, err
			}
			group.CompressedBytes = value
		default:
			if err := r.skip(fieldType); err != nil {
				return group, err
			}
		}
	}
}

func flattenParquetSchema(elements []parquetSchemaElementDetail) ([]parquetSchemaField, bool) {
	if len(elements) == 0 {
		return nil, false
	}
	fields := make([]parquetSchemaField, 0, minInt(len(elements)-1, maxParquetSchemaFields))
	index := 0
	var walk func(parent string, root bool) bool
	walk = func(parent string, root bool) bool {
		if index >= len(elements) {
			return false
		}
		element := elements[index]
		index++
		name := strings.TrimSpace(element.Name)
		currentPath := parent
		if !root {
			if currentPath == "" {
				currentPath = name
			} else if name != "" {
				currentPath += "." + name
			}
			if len(fields) < maxParquetSchemaFields {
				fields = append(fields, parquetSchemaField{
					Path:         currentPath,
					Name:         name,
					PhysicalType: parquetPhysicalTypeName(element),
					LogicalType:  parquetConvertedTypeName(element),
					Repetition:   parquetRepetitionName(element),
					Children:     element.Children,
					TypeLength:   element.TypeLength,
					Precision:    element.Precision,
					Scale:        element.Scale,
				})
			}
		}
		for child := 0; child < element.Children; child++ {
			if !walk(currentPath, false) {
				return false
			}
		}
		return true
	}
	complete := walk("", true)
	truncated := !complete || index < len(elements) || len(elements)-1 > maxParquetSchemaFields
	return fields, truncated
}

func parquetTopLevelColumns(elements []parquetSchemaElementDetail) int {
	if len(elements) == 0 || elements[0].Children < 0 {
		return 0
	}
	return elements[0].Children
}

func parquetPhysicalTypeName(element parquetSchemaElementDetail) string {
	if !element.HasPhysicalType {
		return "GROUP"
	}
	names := []string{"BOOLEAN", "INT32", "INT64", "INT96", "FLOAT", "DOUBLE", "BYTE_ARRAY", "FIXED_LEN_BYTE_ARRAY"}
	if element.PhysicalType >= 0 && element.PhysicalType < int64(len(names)) {
		return names[element.PhysicalType]
	}
	return fmt.Sprintf("TYPE_%d", element.PhysicalType)
}

func parquetRepetitionName(element parquetSchemaElementDetail) string {
	if !element.HasRepetition {
		return ""
	}
	switch element.Repetition {
	case 0:
		return "REQUIRED"
	case 1:
		return "OPTIONAL"
	case 2:
		return "REPEATED"
	default:
		return fmt.Sprintf("REPETITION_%d", element.Repetition)
	}
}

func parquetConvertedTypeName(element parquetSchemaElementDetail) string {
	if !element.HasConvertedType {
		return ""
	}
	names := []string{
		"UTF8", "MAP", "MAP_KEY_VALUE", "LIST", "ENUM", "DECIMAL", "DATE", "TIME_MILLIS",
		"TIME_MICROS", "TIMESTAMP_MILLIS", "TIMESTAMP_MICROS", "UINT_8", "UINT_16", "UINT_32",
		"UINT_64", "INT_8", "INT_16", "INT_32", "INT_64", "JSON", "BSON", "INTERVAL",
	}
	if element.ConvertedType >= 0 && element.ConvertedType < int64(len(names)) {
		return names[element.ConvertedType]
	}
	return fmt.Sprintf("LOGICAL_%d", element.ConvertedType)
}

func parseParquetFileMetadata(data []byte) (parquetSummary, error) {
	detail, err := parseParquetMetadataDetail(data)
	if err != nil {
		return parquetSummary{}, err
	}
	return parquetSummary{Rows: detail.Rows, Columns: parquetTopLevelColumns(detail.Schema)}, nil
}
