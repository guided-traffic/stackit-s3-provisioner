package stackitfake

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// do issues a plain HTTP request against a fake server URL.
func do(t *testing.T, method, url string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestControlPlaneRouting(t *testing.T) {
	s := New("proj", "eu01")
	defer s.Close()
	base := s.CP.URL + "/v2/project/proj/regions/eu01"

	cases := []struct {
		name   string
		method string
		url    string
		body   string
		want   int
	}{
		{"unknown top-level path", http.MethodGet, s.CP.URL + "/nope", "", 404},
		{"foreign project", http.MethodGet, s.CP.URL + "/v2/project/other/regions/eu01/buckets", "", 403},
		{"unknown region", http.MethodGet, s.CP.URL + "/v2/project/proj/regions/eu99/buckets", "", 404},
		{"unknown resource", http.MethodGet, base + "/whatever", "", 404},
		{"service: bad method", http.MethodDelete, base, "", 405},
		{"buckets: bad method", http.MethodPost, base + "/buckets", "", 405},
		{"bucket: bad method", http.MethodPatch, base + "/bucket/foo", "", 405},
		{"get missing bucket", http.MethodGet, base + "/bucket/ghost", "", 404},
		{"group create: bad method", http.MethodGet, base + "/credentials-group", "", 405},
		{"group create: bad payload", http.MethodPost, base + "/credentials-group", "{}", 400},
		{"group delete: bad method", http.MethodGet, base + "/credentials-group/cg-1", "", 405},
		{"groups list: bad method", http.MethodPost, base + "/credentials-groups", "", 405},
		{"key create: bad method", http.MethodGet, base + "/access-key", "", 405},
		{"key delete: bad method", http.MethodGet, base + "/access-key/key-1", "", 405},
		{"keys list: bad method", http.MethodPost, base + "/access-keys", "", 405},
		{"keys list: unknown group", http.MethodGet, base + "/access-keys?credentials-group=cg-ghost", "", 404},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := do(t, tc.method, tc.url, tc.body).StatusCode; got != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.method, tc.url, got, tc.want)
			}
		})
	}
}

func TestDataPlaneRouting(t *testing.T) {
	s := New("proj", "eu01")
	defer s.Close()
	s.SeedBucket("bkt", nil)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		want   int
	}{
		{"missing bucket", http.MethodGet, "/ghost?policy", "", 404},
		{"location probe", http.MethodGet, "/bkt?location", "", 200},
		{"unroutable request", http.MethodGet, "/bkt/some-object", "", 501},
		{"policy: bad method", http.MethodDelete, "/bkt?policy", "", 405},
		{"tagging: bad method", http.MethodDelete, "/bkt?tagging", "", 405},
		{"tagging: malformed xml", http.MethodPut, "/bkt?tagging", "<oops", 400},
		{"versions: bad method", http.MethodPut, "/bkt?versions", "", 405},
		{"list-type: bad method", http.MethodPut, "/bkt?list-type=2", "", 405},
		{"delete: bad method", http.MethodGet, "/bkt?delete", "", 405},
		{"delete: malformed xml", http.MethodPost, "/bkt?delete", "<oops", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := do(t, tc.method, s.S3.URL+tc.path, tc.body).StatusCode; got != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestSeedObjectUnknownBucketPanics(t *testing.T) {
	s := New("proj", "eu01")
	defer s.Close()
	defer func() {
		if recover() == nil {
			t.Error("SeedObject on unknown bucket did not panic")
		}
	}()
	s.SeedObject("ghost", "k", "v", false)
}

func TestInspectionHelpersOnUnknownState(t *testing.T) {
	s := New("proj", "eu01")
	defer s.Close()
	if s.Tags("ghost") != nil {
		t.Error("Tags(ghost) != nil")
	}
	if s.Policy("ghost") != "" {
		t.Error("Policy(ghost) != empty")
	}
	if s.ObjectCount("ghost") != 0 {
		t.Error("ObjectCount(ghost) != 0")
	}
	if s.KeyCount("ghost") != -1 {
		t.Error("KeyCount(ghost) != -1")
	}
	if len(s.BucketNames()) != 0 || len(s.GroupNames()) != 0 {
		t.Error("fresh fake not empty")
	}
}

func ExampleServer() {
	s := New("proj", "eu01")
	defer s.Close()
	s.SeedBucket("demo", map[string]string{"managed-by": "op"})
	fmt.Println(s.BucketNames())
	// Output: [demo]
}
