package manifest

import (
	"encoding/json"
	"fmt"

	"github.com/omnivm/omnivm/pkg/handles"
)

const (
	nativeMemoryBoundary   = "native_memory"
	ownerLifecycleBoundary = "owner_lifecycle"
)

// LifecycleError is a structured owner-side lifecycle diagnostic for stale
// manifest handles. Error keeps the existing human-readable diagnostic while
// BridgeErrorJSON preserves the normalized envelope across C bridge calls.
type LifecycleError struct {
	Runtime             string
	OriginRuntime       string
	Type                string
	Message             string
	BoundaryPath        string
	OriginalErrorHandle string
	Details             interface{}
	DetailsJSON         string
}

func (e *LifecycleError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Message
}

func (e *LifecycleError) ToMap() map[string]interface{} {
	if e == nil {
		return nil
	}
	runtimeName := nonEmpty(e.Runtime, "unknown")
	originRuntime := nonEmpty(e.OriginRuntime, runtimeName)
	errorType := nonEmpty(e.Type, "RuntimeError")
	details := cloneJSONValue(e.Details)
	detailsJSON := e.DetailsJSON
	if detailsJSON == "" && details != nil {
		detailsJSON = jsonString(details)
	}
	return map[string]interface{}{
		"runtime":               runtimeName,
		"origin_runtime":        originRuntime,
		"type":                  errorType,
		"message":               e.Message,
		"traceback":             "",
		"stack_frames":          []interface{}{},
		"cause_chain":           []interface{}{},
		"boundary_path":         e.BoundaryPath,
		"original_error_handle": e.OriginalErrorHandle,
		"details":               details,
		"details_json":          detailsJSON,
	}
}

func (e *LifecycleError) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.ToMap())
}

func (e *LifecycleError) BridgeErrorJSON() ([]byte, error) {
	return e.MarshalJSON()
}

func releasedResourceLifecycleError(id handles.ID, ref *ResourceRef) *LifecycleError {
	runtimeName := "unknown"
	kind := "resource"
	var details interface{}
	if ref != nil {
		runtimeName = nonEmpty(ref.Runtime, "unknown")
		kind = nonEmpty(ref.Kind, "resource")
		details = releasedResourceLifecycleDetails(id, ref, runtimeName, kind)
	}
	message := fmt.Sprintf("manifest HandleCall: closed resource handle %d (runtime=%s kind=%s): owner-side lifecycle is closed", id, runtimeName, kind)
	return &LifecycleError{
		Runtime:       runtimeName,
		OriginRuntime: runtimeName,
		Type:          "RuntimeError",
		Message:       message,
		BoundaryPath:  ownerLifecycleBoundary,
		Details:       details,
		DetailsJSON:   jsonString(details),
	}
}

func releasedStreamLifecycleError(id handles.ID, ref releasedStreamRef) *LifecycleError {
	runtimeName := nonEmpty(ref.Runtime, "unknown")
	kind := nonEmpty(ref.Kind, "stream")
	details := releasedStreamLifecycleDetails(id, runtimeName, kind)
	message := fmt.Sprintf("manifest HandleCall: closed stream handle %d (runtime=%s kind=%s): owner-side lifecycle is closed", id, runtimeName, kind)
	return &LifecycleError{
		Runtime:       runtimeName,
		OriginRuntime: runtimeName,
		Type:          "RuntimeError",
		Message:       message,
		BoundaryPath:  ownerLifecycleBoundary,
		Details:       details,
		DetailsJSON:   jsonString(details),
	}
}

func releasedTableLifecycleError(id handles.ID, ref *TableRef) *LifecycleError {
	runtimeName := "unknown"
	format := "table"
	var details interface{}
	if ref != nil {
		runtimeName = nonEmpty(ref.Runtime, "unknown")
		format = nonEmpty(ref.Format, "table")
		details = releasedTableLifecycleDetails(id, ref, runtimeName, format)
	}
	message := fmt.Sprintf("manifest HandleCall: closed table handle %d (runtime=%s format=%s): owner-side lifecycle is released", id, runtimeName, format)
	return &LifecycleError{
		Runtime:       runtimeName,
		OriginRuntime: runtimeName,
		Type:          "RuntimeError",
		Message:       message,
		BoundaryPath:  nativeMemoryBoundary,
		Details:       details,
		DetailsJSON:   jsonString(details),
	}
}

func releasedTableLifecycleDetails(id handles.ID, ref *TableRef, runtimeName, format string) map[string]interface{} {
	metadata := tableMetadataValue(cloneTableMetadata(ref.Metadata))
	table := map[string]interface{}{
		"id":              uint64(id),
		"runtime":         runtimeName,
		"format":          format,
		"released":        true,
		"lifecycle":       "released",
		"owner_lifecycle": "released",
	}
	if ref.Ownership != "" {
		table["ownership"] = ref.Ownership
	}
	if ref.Release != "" {
		table["release"] = ref.Release
	}
	if metadata != nil {
		table["metadata"] = metadata
		if buffer, _ := metadata["buffer"].(string); buffer != "" {
			table["buffer"] = map[string]interface{}{
				"name":        buffer,
				"state":       "released",
				"lease_state": "released",
				"released":    true,
			}
			if memorySpace, _ := metadata["memory_space"].(string); memorySpace != "" {
				table["buffer"].(map[string]interface{})["memory_space"] = memorySpace
			}
		}
	}
	return map[string]interface{}{"table": table}
}

func releasedResourceLifecycleDetails(id handles.ID, ref *ResourceRef, runtimeName, kind string) map[string]interface{} {
	resource := map[string]interface{}{
		"id":              uint64(id),
		"runtime":         runtimeName,
		"kind":            kind,
		"closed":          true,
		"lifecycle":       "closed",
		"owner_lifecycle": "closed",
	}
	if ref.Disposer != "" {
		resource["disposer"] = ref.Disposer
	}
	return map[string]interface{}{"resource": resource}
}

func releasedStreamLifecycleDetails(id handles.ID, runtimeName, kind string) map[string]interface{} {
	return map[string]interface{}{
		"stream": map[string]interface{}{
			"id":              uint64(id),
			"runtime":         runtimeName,
			"kind":            kind,
			"closed":          true,
			"lifecycle":       "closed",
			"owner_lifecycle": "closed",
		},
	}
}
