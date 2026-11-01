// Package clienttest provides synthetic qURL API response fixtures.
package clienttest

// ResourceDetailData mirrors qurl-service's ResourceDetailResponse data field.
func ResourceDetailData(resourceID, resourceType, slug string) map[string]any {
	return map[string]any{
		"resource": map[string]any{
			"resource_id": resourceID,
			"type":        resourceType,
			"slug":        slug,
			"status":      "active",
		},
		"qurls": []any{},
	}
}
