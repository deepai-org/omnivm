package manifest

import (
	"strconv"
	"strings"
)

const descriptorInternalKeysMarker = "__OMNIVM_DESCRIPTOR_INTERNAL_KEYS__"

var descriptorInternalKeyGroups = [][]string{
	{"__omnivm_resource__", "__omnivm_table__", "__omnivm_job__", "__omnivm_materialized__"},
	{"id", "runtime", "kind", "closed", "transfer", "disposer"},
	{"format", "ownership", "metadata", "buffer", "released"},
	{"done", "cancelled", "cancelReason", "payload", "result"},
}

var descriptorInternalKeySet = func() map[string]struct{} {
	set := make(map[string]struct{})
	for _, group := range descriptorInternalKeyGroups {
		for _, key := range group {
			set[key] = struct{}{}
		}
	}
	return set
}()

func isDescriptorPayload(value map[string]interface{}) bool {
	if value == nil {
		return false
	}
	return value["__omnivm_resource__"] == true ||
		value["__omnivm_table__"] == true ||
		value["__omnivm_job__"] == true
}

func isDescriptorInternalKey(key string) bool {
	_, ok := descriptorInternalKeySet[key]
	return ok
}

func descriptorInternalKeysTupleLiteral(indent string) string {
	return descriptorInternalKeysJoined(", ", ",\n"+indent, func(key string) string {
		return strconv.Quote(key)
	})
}

func descriptorInternalKeysJSPredicate(name string, indent string) string {
	return descriptorInternalKeysJoined(" || ", "\n"+indent+"|| ", func(key string) string {
		return name + " === " + strconv.Quote(key)
	})
}

func descriptorInternalKeysJoined(itemSep, groupSep string, format func(string) string) string {
	groups := make([]string, 0, len(descriptorInternalKeyGroups))
	for _, group := range descriptorInternalKeyGroups {
		items := make([]string, 0, len(group))
		for _, key := range group {
			items = append(items, format(key))
		}
		groups = append(groups, strings.Join(items, itemSep))
	}
	return strings.Join(groups, groupSep)
}
