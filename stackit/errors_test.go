package stackit

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/stackitcloud/stackit-sdk-go/core/oapierror"
)

// Bodies captured from the live StackIT Object Storage API (region eu01) on
// 2026-08-25 via GetServiceStatus. They are the reference the classification in
// errors.go is built against, so the tests use them verbatim rather than
// paraphrasing them.
const (
	bodyNotEnabled = `{"detail":[{"key":"project.not_found","msg":"The project could not be found"}]}`
	bodyForbidden  = `{"timestamp":"2026-08-25T11:46:25Z","path":"/v2/project/00000000-0000-4000-8000-000000000000/regions/eu01","status":403,"error":"Forbidden","message":"Unauthorized"}`
	// bodyGatewayHTML stands in for the nginx/WAF error page observed on
	// 2026-08-25 08:13 UTC, which the SDK wraps in the very same error type
	// carrying status 403.
	bodyGatewayHTML = "<html>\r\n<head><title>403 Forbidden</title></head>\r\n<body>\r\n<center><h1>403 Forbidden</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"
	// bodyRevokedKey is what the STACKIT token endpoint
	// (https://accounts.stackit.cloud/oauth/v2/token) answers when the
	// service-account key backing the assertion no longer exists. Measured live
	// on 2026-08-25: HTTP 400, Content-Type application/json. The SDK stamps that
	// status and body into the same GenericOpenAPIError it uses for API errors,
	// so a revoked key reaches the operator looking like a 400 "API answer" — and
	// never as the 401/403 one would expect.
	bodyRevokedKey = `{"error":"invalid_grant"}`
)

// apiErr builds the error shape the SDK produces for a non-2xx response: status
// code plus the raw body, exactly as api_default.go constructs it.
func apiErr(status int, body string) error {
	return &oapierror.GenericOpenAPIError{
		StatusCode:   status,
		Body:         []byte(body),
		ErrorMessage: http.StatusText(status),
	}
}

// TestProviderRefused pins the one distinction the whole transient/definitive
// split rests on: a structured JSON refusal by the provider is definitive, an
// intermediary answering with the same status code is not.
func TestProviderRefused(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"structured 403 from the API", apiErr(http.StatusForbidden, bodyForbidden), true},
		{"structured 401 from the API", apiErr(http.StatusUnauthorized, `{"message":"Unauthorized"}`), true},
		{"gateway HTML page carrying 403", apiErr(http.StatusForbidden, bodyGatewayHTML), false},
		{"403 with an empty body", apiErr(http.StatusForbidden, ""), false},
		{"structured 404 not enabled", apiErr(http.StatusNotFound, bodyNotEnabled), false},
		{"structured 500", apiErr(http.StatusInternalServerError, `{"message":"boom"}`), false},
		{"plain transport error", io.ErrUnexpectedEOF, false},
		{"nil error", nil, false},
		{"wrapped structured 403", fmt.Errorf("enable object storage: %w", apiErr(http.StatusForbidden, bodyForbidden)), true},
		// The revoked-key path. Without this case the operator would report the
		// whole fleet Ready while every credential it holds is dead.
		{"revoked key: token endpoint 400 invalid_grant", apiErr(http.StatusBadRequest, bodyRevokedKey), true},
		{"revoked key wrapped by the SDK and the reconciler", fmt.Errorf(
			"check object storage status in project p: %w",
			apiErr(http.StatusBadRequest, bodyRevokedKey)), true},
		{"gateway HTML page carrying 400", apiErr(http.StatusBadRequest, bodyGatewayHTML), false},
		{"400 with an empty body", apiErr(http.StatusBadRequest, ""), false},
		{"503 during a blip", apiErr(http.StatusServiceUnavailable, `{"message":"unavailable"}`), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderRefused(tc.err); got != tc.want {
				t.Fatalf("ProviderRefused() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsServiceNotEnabled guards the EnsureService fall-through: only a
// structured 404 may lead to an EnableService call. Everything else leaves the
// status unknown, and unknown must not be read as disabled.
func TestIsServiceNotEnabled(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"structured 404 not enabled", apiErr(http.StatusNotFound, bodyNotEnabled), true},
		{"structured 403 not authorized", apiErr(http.StatusForbidden, bodyForbidden), false},
		{"gateway HTML page carrying 404", apiErr(http.StatusNotFound, bodyGatewayHTML), false},
		{"404 with an empty body", apiErr(http.StatusNotFound, ""), false},
		{"503 from an intermediary", apiErr(http.StatusServiceUnavailable, bodyGatewayHTML), false},
		{"plain transport error", io.ErrUnexpectedEOF, false},
		{"nil error", nil, false},
		{"wrapped structured 404", fmt.Errorf("check status: %w", apiErr(http.StatusNotFound, bodyNotEnabled)), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isServiceNotEnabled(tc.err); got != tc.want {
				t.Fatalf("isServiceNotEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestModelIsNotADiscriminator documents why errors.go keys off the raw body and
// not oapierror.Model: the SDK decodes every error body into ErrorMessage, which
// only knows a Detail field, so the authorization gateway's differently shaped
// JSON decodes into an empty struct without failing. A Model-based check would
// therefore misread a real 403 as unstructured.
func TestModelIsNotADiscriminator(t *testing.T) {
	// Both are genuine API answers; only one populates Detail.
	forbidden := apiErr(http.StatusForbidden, bodyForbidden)
	notEnabled := apiErr(http.StatusNotFound, bodyNotEnabled)

	var fe, ne *oapierror.GenericOpenAPIError
	if !errors.As(forbidden, &fe) || !errors.As(notEnabled, &ne) {
		t.Fatal("test fixtures are not GenericOpenAPIError")
	}
	if fe.GetModel() != nil {
		t.Fatalf("fixture unexpectedly carries a model: %#v", fe.GetModel())
	}
	// The property that matters: the raw body separates them, and both count as
	// structured answers.
	if !ProviderRefused(forbidden) {
		t.Error("structured 403 must be a definitive refusal")
	}
	if !isServiceNotEnabled(notEnabled) {
		t.Error("structured 404 must be the not-enabled answer")
	}
}
