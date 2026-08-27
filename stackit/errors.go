package stackit

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/stackitcloud/stackit-sdk-go/core/oapierror"
)

// The StackIT Object Storage API answers with distinguishable, structured JSON
// for every case the operator has to tell apart. Verified against the live API
// on 2026-08-25 (GetServiceStatus, region eu01):
//
//	enabled        200 {"project":"<uuid>","scope":"PUBLIC"}
//	not enabled    404 {"detail":[{"key":"project.not_found","msg":"The project could not be found"}]}
//	not authorized 403 {"timestamp":"…","path":"…","status":403,"error":"Forbidden","message":"Unauthorized"}
//
// A revoked or deleted service-account key does NOT produce any of those: the
// key flow never gets a token, so the failure comes from the token endpoint
// instead. Also verified live on 2026-08-25 against
// https://accounts.stackit.cloud/oauth/v2/token:
//
//	revoked key    400 {"error":"invalid_grant"}   (RFC 6749 §5.2)
//
// The SDK stamps the token endpoint's status and body into the same
// *oapierror.GenericOpenAPIError it uses for API errors
// (core/clients/auth_flow.go parseTokenResponse), so both arrive here in one
// shape — which is why the status set below includes 400.
//
// Everything in this file keys off that distinction. The decisive test is not
// the status code alone but whether the body is valid JSON, because a gateway
// or WAF in front of either endpoint produces the very same error type carrying
// its own status code and an HTML body. That is what happened on 2026-08-25: a
// 403 HTML page from an intermediary was indistinguishable from a real refusal
// by status code alone.
//
// Note that oapierror.Model is NOT a usable discriminator. The SDK decodes error
// bodies into objectstorage.ErrorMessage, which only has a Detail field, so the
// authorization gateway's differently shaped JSON decodes "successfully" into an
// empty struct. Only the raw body tells the two apart.

// apiAnswer reports whether err carries an answer from the provider — a
// GenericOpenAPIError whose body is valid JSON — and returns its status code.
// A transport-level failure, or an HTML error page from an intermediary, is not
// an answer: nothing on the provider side got to decide, so ok is false.
func apiAnswer(err error) (status int, ok bool) {
	var apiErr *oapierror.GenericOpenAPIError
	if !errors.As(err, &apiErr) {
		return 0, false
	}
	// An empty body is not a structured answer either. Treating it as one would
	// make an intermediary that drops the body look authoritative.
	if !json.Valid(apiErr.GetBody()) {
		return 0, false
	}
	return apiErr.GetStatusCode(), true
}

// ProviderRefused reports whether err is the provider's own structured refusal
// of the request — an answer that repeating the request cannot change. These are
// the only provider failures the operator treats as definitive rather than as a
// failure to reach the provider.
//
// It matches three cases, all as structured JSON:
//
//   - 401/403 — the Object Storage API refusing an authenticated request, e.g.
//     the service account lost its role on the project.
//   - 400 — either the token endpoint rejecting the service-account key
//     (`invalid_grant`, how a revoked or deleted key surfaces) or the API
//     rejecting the request itself. Neither is retryable.
//
// The 400 case is why this is not simply an auth check: a revoked key never
// reaches the Object Storage API at all, so it can only be recognised here.
// Missing it would leave the entire fleet reporting Ready while every credential
// it holds is dead — the one degradation that must never be masked.
//
// A gateway or WAF page carrying any of these status codes is deliberately not
// matched. It is a failure in front of the provider, not a decision by it.
func ProviderRefused(err error) bool {
	status, ok := apiAnswer(err)
	if !ok {
		return false
	}
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return true
	default:
		return false
	}
}

// isServiceNotEnabled reports whether err is the API's definitive answer that
// Object Storage is not enabled for the project: a structured JSON 404.
//
// Anything else — 403, 5xx, a gateway page, a network error — leaves the service
// status unknown. Unknown must never be read as "disabled": doing so turns a
// failed read into an attempted write (EnableService) on every reconcile of
// every Bucket, which is exactly how a two-minute provider blip on 2026-08-25
// produced 342 reconcile errors.
func isServiceNotEnabled(err error) bool {
	status, ok := apiAnswer(err)
	return ok && status == http.StatusNotFound
}
