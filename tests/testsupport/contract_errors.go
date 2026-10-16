package testsupport

// ContractErrorCodes is the closed set of values of the public v2 error code,
// components.schemas.Error.properties.code.enum.
//
// Source: umir-factory/docs/api/completion-v1-composed-private.json
// (git blob 0083c4b3a8079536fa4ef2f03579631baff7ad74, last changed by umir-factory commit
// 80c240065892153129b590f4bdbd2411b7c0319a, "fix(contract): preserve catalog
// names and map planning capacity"). The file is not on the current
// umir-factory checkout; it is read from the durable worktrees, where it is
// identical. Update this list together with that file.
var ContractErrorCodes = []string{
	"InvalidArgument",
	"Unauthenticated",
	"InvalidOrigin",
	"InvalidCsrfToken",
	"Forbidden",
	"AuthUnavailable",
	"AuthStorageUnavailable",
	"AuthUpdateOutcomeUnknown",
	"CommandConflict",
	"LastAdmin",
	"ScopeDeleted",
	"RuntimeNotReady",
	"RuntimeReset",
	"SettingsChanged",
	"UnknownTimeZone",
	"TimeZoneDataUnavailable",
	"AmbiguousLocalTime",
	"NonexistentLocalTime",
	"SkippedDate",
	"RefreshRequired",
	"UnknownEventSchema",
	"CalculationUnavailable",
	"ProductConflict",
	"DishConflict",
	"BatchNotFound",
	"NotPendingWriteoff",
	"AlreadyWrittenOff",
	"InvitationExpired",
	"InvitationRevoked",
	"RateLimited",
	"ChallengeLocked",
	"InvalidProof",
	"DeliveryUnavailable",
	"UpstreamUnavailable",
	"InvalidRequest",
	"InvalidCredentials",
	"CurrentPasswordIncorrect",
	"NotFound",
	"MethodNotAllowed",
	"PayloadTooLarge",
	"UnsupportedMediaType",
	"InvalidDisplayName",
	"PasswordRejected",
	"LoginRateLimited",
	"PasswordChangeRateLimited",
	"FeedbackRateLimited",
	"SessionStorageUnavailable",
	"SessionUpdateOutcomeUnknown",
	"AccountStorageUnavailable",
	"AccountUpdateOutcomeUnknown",
	"ProposalNotFound",
	"InventoryUnavailable",
	"TechnicalError",
	"UpstreamTimeout",
	"ServiceUnavailable",
	"InvalidToken",
	"LimitExceeded",
}

// IsContractErrorCode reports whether code is in the public v2 error code enum.
func IsContractErrorCode(code string) bool {
	for _, known := range ContractErrorCodes {
		if known == code {
			return true
		}
	}
	return false
}
