package access

// ValidateAuditResourceFilter requires a resource type for any exact resource ID.
// Resources are audit metadata: validation never looks up their current existence.
func ValidateAuditResourceFilter(resourceType, resourceID string) error {
	if resourceType != "" && !auditActionPattern.MatchString(resourceType) {
		return ErrInvalidAuditQuery
	}
	if resourceID != "" && (resourceType == "" || !legalHoldUUIDPattern.MatchString(resourceID)) {
		return ErrInvalidAuditQuery
	}
	return nil
}
