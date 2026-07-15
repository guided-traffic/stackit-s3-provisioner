// Package stackitfake provides an in-memory fake of the two StackIT surfaces
// the operator talks to — the Object Storage control-plane REST API (consumed
// via the STACKIT SDK) and the S3 data plane (consumed via minio-go) — served
// over local httptest servers. It exists so the stackit client wrapper and the
// Bucket reconciler can be exercised end-to-end in offline unit tests.
//
// Fidelity notes (mirrors verified real-API behavior, see INIT-SETUP.md):
//   - a foreign projectId yields 403 (Layer-1 isolation),
//   - creating an existing bucket yields 409,
//   - deleting a non-empty bucket yields 409,
//   - deleting a credentials group that still has access keys yields 422,
//   - deleting an access key without the credentials-group query param yields 500,
//   - a bucket without tags/policy yields NoSuchTagSet / NoSuchBucketPolicy.
package stackitfake

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Object is one stored object version in a fake bucket.
type Object struct {
	Key          string
	VersionID    string
	DeleteMarker bool
}

// accessKey is one S3 credential in a credentials group.
type accessKey struct {
	KeyID           string
	AccessKeyID     string
	SecretAccessKey string
}

// group is a credentials group.
type group struct {
	ID          string
	URN         string
	DisplayName string
	Keys        []accessKey
}

// bucket is one fake bucket with its data-plane state.
type bucket struct {
	Name    string
	Objects []Object
	Tags    map[string]string
	Policy  string
}

// Server is the in-memory fake. CP serves the control-plane REST API, S3 the
// data plane. All state is shared between the two, like in the real service.
type Server struct {
	ProjectID string
	Region    string

	CP *httptest.Server
	S3 *httptest.Server

	mu             sync.Mutex
	serviceEnabled bool
	buckets        map[string]*bucket
	groups         map[string]*group
	seq            int
	failNext       map[string]int
}

// New starts a fake with the service already enabled (the common case).
func New(projectID, region string) *Server {
	s := &Server{
		ProjectID:      projectID,
		Region:         region,
		serviceEnabled: true,
		buckets:        map[string]*bucket{},
		groups:         map[string]*group{},
		failNext:       map[string]int{},
	}
	s.CP = httptest.NewServer(http.HandlerFunc(s.controlPlane))
	s.S3 = httptest.NewServer(http.HandlerFunc(s.dataPlane))
	return s
}

// Close shuts both servers down.
func (s *Server) Close() {
	s.CP.Close()
	s.S3.Close()
}

// DisableService flips the project to "object storage not enabled" so
// EnsureService has to enable it.
func (s *Server) DisableService() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serviceEnabled = false
}

// FailNext makes the next call of the given operation fail with the given HTTP
// status. Operations: ServiceStatus, EnableService, CreateBucket, GetBucket,
// DeleteBucket, ListBuckets, CreateGroup, DeleteGroup, ListGroups, CreateKey,
// DeleteKey, ListKeys, S3ListObjects, S3Delete, S3GetTagging, S3PutTagging,
// S3GetPolicy, S3PutPolicy.
//
// Caveat for the S3* operations: minio-go transparently retries retryable
// failures (e.g. 500/InternalError), consuming the injection and then
// succeeding — inject 403 (served as AccessDenied, non-retryable) when a test
// needs the error to actually surface to the caller.
func (s *Server) FailNext(op string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[op] = status
}

// failFor consumes a pending failure injection for op. Caller must hold mu.
func (s *Server) failFor(op string) (int, bool) {
	if st, ok := s.failNext[op]; ok {
		delete(s.failNext, op)
		return st, true
	}
	return 0, false
}

// SeedBucket creates a bucket directly in the fake (bypassing the API), e.g. a
// pre-existing foreign bucket. tags may be nil (untagged bucket).
func (s *Server) SeedBucket(name string, tags map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := &bucket{Name: name, Tags: map[string]string{}}
	for k, v := range tags {
		b.Tags[k] = v
	}
	s.buckets[name] = b
}

// SeedObject stores an object version in a bucket (bucket must exist).
func (s *Server) SeedObject(bucketName, key, versionID string, deleteMarker bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[bucketName]
	if b == nil {
		panic(fmt.Sprintf("stackitfake: SeedObject on unknown bucket %q", bucketName))
	}
	b.Objects = append(b.Objects, Object{Key: key, VersionID: versionID, DeleteMarker: deleteMarker})
}

// BucketNames returns the names of all existing buckets, sorted.
func (s *Server) BucketNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.buckets))
	for n := range s.buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ObjectCount returns the number of stored object versions in a bucket (0 for
// an unknown bucket).
func (s *Server) ObjectCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.buckets[name]; b != nil {
		return len(b.Objects)
	}
	return 0
}

// Tags returns a copy of a bucket's tag set (nil for an unknown bucket).
func (s *Server) Tags(name string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.buckets[name]
	if b == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range b.Tags {
		out[k] = v
	}
	return out
}

// Policy returns a bucket's policy JSON ("" when unset or bucket unknown).
func (s *Server) Policy(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.buckets[name]; b != nil {
		return b.Policy
	}
	return ""
}

// GroupNames returns the display names of all credentials groups, sorted.
func (s *Server) GroupNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.groups))
	for _, g := range s.groups {
		names = append(names, g.DisplayName)
	}
	sort.Strings(names)
	return names
}

// KeyCount returns the number of access keys in the group with the given
// display name (-1 when no such group exists).
func (s *Server) KeyCount(displayName string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.groups {
		if g.DisplayName == displayName {
			return len(g.Keys)
		}
	}
	return -1
}

// nextID returns a fresh sequence-numbered id. Caller must hold mu.
func (s *Server) nextID(prefix string) string {
	s.seq++
	return fmt.Sprintf("%s-%d", prefix, s.seq)
}

// bucketURL is the path-style URL of a bucket on the fake S3 server.
func (s *Server) bucketURL(name string) string {
	return s.S3.URL + "/" + name
}

// JSON field names of the control-plane API responses (named to satisfy
// repeated-literal linting and to keep the response shape in one place).
const (
	fieldProject     = "project"
	fieldDisplayName = "displayName"
	fieldBucket      = "bucket"
)

// --- control plane ---------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

func (s *Server) bucketJSON(b *bucket) map[string]any {
	return map[string]any{
		"name":                  b.Name,
		"region":                s.Region,
		"urlPathStyle":          s.bucketURL(b.Name),
		"urlVirtualHostedStyle": s.bucketURL(b.Name),
		"objectLockEnabled":     false,
	}
}

func groupJSON(g *group) map[string]any {
	return map[string]any{
		"credentialsGroupId": g.ID,
		fieldDisplayName:     g.DisplayName,
		"urn":                g.URN,
	}
}

// controlPlane routes the STACKIT Object Storage REST API
// (/v2/project/{pid}/regions/{region}/...).
func (s *Server) controlPlane(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rest, ok := s.splitCPPath(w, r.URL.Path)
	if !ok {
		return
	}

	switch {
	case rest == "":
		s.handleService(w, r)
	case rest == "buckets":
		s.handleListBuckets(w, r)
	case strings.HasPrefix(rest, "bucket/"):
		s.handleBucket(w, r, strings.TrimPrefix(rest, "bucket/"))
	case rest == "credentials-group":
		s.handleCreateGroup(w, r)
	case strings.HasPrefix(rest, "credentials-group/"):
		s.handleDeleteGroup(w, r, strings.TrimPrefix(rest, "credentials-group/"))
	case rest == "credentials-groups":
		s.handleListGroups(w, r)
	case rest == "access-key":
		s.handleCreateKey(w, r)
	case strings.HasPrefix(rest, "access-key/"):
		s.handleDeleteKey(w, r, strings.TrimPrefix(rest, "access-key/"))
	case rest == "access-keys":
		s.handleListKeys(w, r)
	default:
		apiError(w, http.StatusNotFound, "unknown path "+r.URL.Path)
	}
}

// splitCPPath validates /v2/project/{pid}/regions/{region}[/...] and returns
// the trailing resource path. A foreign project yields 403, mirroring Layer 1.
// Caller must hold mu.
func (s *Server) splitCPPath(w http.ResponseWriter, path string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/v2/project/")
	parts := strings.SplitN(trimmed, "/", 4)
	if trimmed == path || len(parts) < 3 || parts[1] != "regions" {
		apiError(w, http.StatusNotFound, "unknown path "+path)
		return "", false
	}
	if parts[0] != s.ProjectID {
		apiError(w, http.StatusForbidden, "no permission on project "+parts[0])
		return "", false
	}
	if parts[2] != s.Region {
		apiError(w, http.StatusNotFound, "unknown region "+parts[2])
		return "", false
	}
	if len(parts) == 4 {
		return parts[3], true
	}
	return "", true
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if st, ok := s.failFor("ServiceStatus"); ok {
			apiError(w, st, "injected ServiceStatus failure")
			return
		}
		if !s.serviceEnabled {
			apiError(w, http.StatusNotFound, "object storage not enabled")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID})
	case http.MethodPost:
		if st, ok := s.failFor("EnableService"); ok {
			apiError(w, st, "injected EnableService failure")
			return
		}
		s.serviceEnabled = true
		writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID})
	default:
		apiError(w, http.StatusMethodNotAllowed, r.Method)
	}
}

func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, r.Method)
		return
	}
	if st, ok := s.failFor("ListBuckets"); ok {
		apiError(w, st, "injected ListBuckets failure")
		return
	}
	names := make([]string, 0, len(s.buckets))
	for n := range s.buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	list := make([]any, 0, len(names))
	for _, n := range names {
		list = append(list, s.bucketJSON(s.buckets[n]))
	}
	writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID, "buckets": list})
}

func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, name string) {
	b := s.buckets[name]
	switch r.Method {
	case http.MethodPost:
		if st, ok := s.failFor("CreateBucket"); ok {
			apiError(w, st, "injected CreateBucket failure")
			return
		}
		if b != nil {
			apiError(w, http.StatusConflict, "bucket already exists")
			return
		}
		s.buckets[name] = &bucket{Name: name, Tags: map[string]string{}}
		writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID, fieldBucket: name})
	case http.MethodGet:
		if st, ok := s.failFor("GetBucket"); ok {
			apiError(w, st, "injected GetBucket failure")
			return
		}
		if b == nil {
			apiError(w, http.StatusNotFound, "bucket not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID, fieldBucket: s.bucketJSON(b)})
	case http.MethodDelete:
		if st, ok := s.failFor("DeleteBucket"); ok {
			apiError(w, st, "injected DeleteBucket failure")
			return
		}
		if b == nil {
			apiError(w, http.StatusNotFound, "bucket not found")
			return
		}
		if len(b.Objects) > 0 {
			apiError(w, http.StatusConflict, "bucket is not empty")
			return
		}
		delete(s.buckets, name)
		writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID, fieldBucket: name})
	default:
		apiError(w, http.StatusMethodNotAllowed, r.Method)
	}
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, http.StatusMethodNotAllowed, r.Method)
		return
	}
	if st, ok := s.failFor("CreateGroup"); ok {
		apiError(w, st, "injected CreateGroup failure")
		return
	}
	var payload struct {
		DisplayName string `json:"displayName"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &payload); err != nil || payload.DisplayName == "" {
		apiError(w, http.StatusBadRequest, "missing displayName")
		return
	}
	id := s.nextID("cg")
	g := &group{
		ID:          id,
		URN:         fmt.Sprintf("urn:sgws:identity::%s:group/%s", s.ProjectID, id),
		DisplayName: payload.DisplayName,
	}
	s.groups[id] = g
	writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID, "credentialsGroup": groupJSON(g)})
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodDelete {
		apiError(w, http.StatusMethodNotAllowed, r.Method)
		return
	}
	if st, ok := s.failFor("DeleteGroup"); ok {
		apiError(w, st, "injected DeleteGroup failure")
		return
	}
	g := s.groups[id]
	if g == nil {
		apiError(w, http.StatusNotFound, "credentials group not found")
		return
	}
	if len(g.Keys) > 0 {
		apiError(w, http.StatusUnprocessableEntity, "credentials group still has access keys")
		return
	}
	delete(s.groups, id)
	writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID})
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, r.Method)
		return
	}
	if st, ok := s.failFor("ListGroups"); ok {
		apiError(w, st, "injected ListGroups failure")
		return
	}
	ids := make([]string, 0, len(s.groups))
	for id := range s.groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	list := make([]any, 0, len(ids))
	for _, id := range ids {
		list = append(list, groupJSON(s.groups[id]))
	}
	writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID, "credentialsGroups": list})
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		apiError(w, http.StatusMethodNotAllowed, r.Method)
		return
	}
	if st, ok := s.failFor("CreateKey"); ok {
		apiError(w, st, "injected CreateKey failure")
		return
	}
	g := s.groups[r.URL.Query().Get("credentials-group")]
	if g == nil {
		apiError(w, http.StatusNotFound, "credentials group not found")
		return
	}
	id := s.nextID("key")
	ak := accessKey{
		KeyID:           id,
		AccessKeyID:     "AK" + id,
		SecretAccessKey: "SK" + id,
	}
	g.Keys = append(g.Keys, ak)
	writeJSON(w, http.StatusOK, map[string]any{
		fieldProject:      s.ProjectID,
		"keyId":           ak.KeyID,
		"accessKey":       ak.AccessKeyID,
		"secretAccessKey": ak.SecretAccessKey,
		fieldDisplayName:  ak.AccessKeyID,
		"expires":         "",
	})
}

func (s *Server) handleDeleteKey(w http.ResponseWriter, r *http.Request, keyID string) {
	if r.Method != http.MethodDelete {
		apiError(w, http.StatusMethodNotAllowed, r.Method)
		return
	}
	if st, ok := s.failFor("DeleteKey"); ok {
		apiError(w, st, "injected DeleteKey failure")
		return
	}
	// Real-API fidelity: deleting a key without its group id is a 500.
	g := s.groups[r.URL.Query().Get("credentials-group")]
	if g == nil {
		apiError(w, http.StatusInternalServerError, "credentials-group query parameter required")
		return
	}
	for i, k := range g.Keys {
		if k.KeyID == keyID {
			g.Keys = append(g.Keys[:i], g.Keys[i+1:]...)
			writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID})
			return
		}
	}
	apiError(w, http.StatusNotFound, "access key not found")
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		apiError(w, http.StatusMethodNotAllowed, r.Method)
		return
	}
	if st, ok := s.failFor("ListKeys"); ok {
		apiError(w, st, "injected ListKeys failure")
		return
	}
	g := s.groups[r.URL.Query().Get("credentials-group")]
	if g == nil {
		apiError(w, http.StatusNotFound, "credentials group not found")
		return
	}
	list := make([]any, 0, len(g.Keys))
	for _, k := range g.Keys {
		list = append(list, map[string]any{"keyId": k.KeyID, fieldDisplayName: k.AccessKeyID, "expires": ""})
	}
	writeJSON(w, http.StatusOK, map[string]any{fieldProject: s.ProjectID, "accessKeys": list})
}

// --- data plane (S3) --------------------------------------------------------

const s3FixedTime = "2024-01-01T00:00:00.000Z"

func s3Error(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	// Test-only fake serving fixed strings to an S3 SDK on localhost — no
	// browser ever renders this response.
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, msg) // #nosec G705

}

func writeXML(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+body)
}

// dataPlane routes path-style S3 requests (/{bucket}[/{key}]?...).
func (s *Server) dataPlane(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	b := s.buckets[name]
	if b == nil {
		s3Error(w, http.StatusNotFound, "NoSuchBucket", "bucket "+name+" does not exist")
		return
	}

	switch {
	case q.Has("location"):
		writeXML(w, `<LocationConstraint>`+s.Region+`</LocationConstraint>`)
	case q.Has("policy"):
		s.handlePolicy(w, r, b)
	case q.Has("tagging"):
		s.handleTagging(w, r, b)
	case q.Has("versions"):
		s.handleListVersions(w, r, b)
	case q.Has("delete"):
		s.handleMultiDelete(w, r, b)
	case q.Has("list-type"):
		s.handleListObjectsV2(w, r, b, q)
	default:
		s3Error(w, http.StatusNotImplemented, "NotImplemented", r.Method+" "+r.URL.String())
	}
}

func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request, b *bucket) {
	switch r.Method {
	case http.MethodGet:
		if st, ok := s.failFor("S3GetPolicy"); ok {
			s3Error(w, st, "AccessDenied", "injected S3GetPolicy failure")
			return
		}
		if b.Policy == "" {
			s3Error(w, http.StatusNotFound, "NoSuchBucketPolicy", "no policy on "+b.Name)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, b.Policy)
	case http.MethodPut:
		if st, ok := s.failFor("S3PutPolicy"); ok {
			s3Error(w, st, "AccessDenied", "injected S3PutPolicy failure")
			return
		}
		body, _ := io.ReadAll(r.Body)
		b.Policy = string(body)
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

// taggingDoc mirrors the S3 bucket-tagging XML document.
type taggingDoc struct {
	XMLName xml.Name `xml:"Tagging"`
	TagSet  struct {
		Tags []struct {
			Key   string `xml:"Key"`
			Value string `xml:"Value"`
		} `xml:"Tag"`
	} `xml:"TagSet"`
}

func (s *Server) handleTagging(w http.ResponseWriter, r *http.Request, b *bucket) {
	switch r.Method {
	case http.MethodGet:
		if st, ok := s.failFor("S3GetTagging"); ok {
			s3Error(w, st, "AccessDenied", "injected S3GetTagging failure")
			return
		}
		if len(b.Tags) == 0 {
			s3Error(w, http.StatusNotFound, "NoSuchTagSet", "no tags on "+b.Name)
			return
		}
		keys := make([]string, 0, len(b.Tags))
		for k := range b.Tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteString(`<Tagging><TagSet>`)
		for _, k := range keys {
			fmt.Fprintf(&sb, `<Tag><Key>%s</Key><Value>%s</Value></Tag>`, k, b.Tags[k])
		}
		sb.WriteString(`</TagSet></Tagging>`)
		writeXML(w, sb.String())
	case http.MethodPut:
		if st, ok := s.failFor("S3PutTagging"); ok {
			s3Error(w, st, "AccessDenied", "injected S3PutTagging failure")
			return
		}
		body, _ := io.ReadAll(r.Body)
		var doc taggingDoc
		if err := xml.Unmarshal(body, &doc); err != nil {
			s3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
			return
		}
		b.Tags = map[string]string{}
		for _, t := range doc.TagSet.Tags {
			b.Tags[t.Key] = t.Value
		}
		w.WriteHeader(http.StatusOK)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request, b *bucket) {
	if r.Method != http.MethodGet {
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
		return
	}
	if st, ok := s.failFor("S3ListObjects"); ok {
		s3Error(w, st, "AccessDenied", "injected S3ListObjects failure")
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `<ListVersionsResult><Name>%s</Name><IsTruncated>false</IsTruncated>`, b.Name)
	for _, o := range b.Objects {
		tag := "Version"
		if o.DeleteMarker {
			tag = "DeleteMarker"
		}
		fmt.Fprintf(&sb,
			`<%[1]s><Key>%[2]s</Key><VersionId>%[3]s</VersionId><IsLatest>false</IsLatest><LastModified>%[4]s</LastModified><ETag>"0"</ETag><Size>1</Size></%[1]s>`,
			tag, o.Key, o.VersionID, s3FixedTime)
	}
	sb.WriteString(`</ListVersionsResult>`)
	writeXML(w, sb.String())
}

func (s *Server) handleListObjectsV2(w http.ResponseWriter, r *http.Request, b *bucket, q url.Values) {
	if r.Method != http.MethodGet {
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
		return
	}
	if st, ok := s.failFor("S3ListObjects"); ok {
		s3Error(w, st, "AccessDenied", "injected S3ListObjects failure")
		return
	}
	maxKeys := len(b.Objects)
	if mk := q.Get("max-keys"); mk != "" {
		if n, err := strconv.Atoi(mk); err == nil && n < maxKeys {
			maxKeys = n
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, `<ListBucketResult><Name>%s</Name><IsTruncated>false</IsTruncated>`, b.Name)
	count := 0
	for _, o := range b.Objects {
		if o.DeleteMarker || count == maxKeys {
			continue
		}
		count++
		fmt.Fprintf(&sb,
			`<Contents><Key>%s</Key><LastModified>%s</LastModified><ETag>"0"</ETag><Size>1</Size></Contents>`,
			o.Key, s3FixedTime)
	}
	sb.WriteString(`</ListBucketResult>`)
	writeXML(w, sb.String())
}

// deleteDoc mirrors the S3 multi-object-delete request XML.
type deleteDoc struct {
	XMLName xml.Name `xml:"Delete"`
	Objects []struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId"`
	} `xml:"Object"`
}

func (s *Server) handleMultiDelete(w http.ResponseWriter, r *http.Request, b *bucket) {
	if r.Method != http.MethodPost {
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
		return
	}
	if st, ok := s.failFor("S3Delete"); ok {
		s3Error(w, st, "AccessDenied", "injected S3Delete failure")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var doc deleteDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		s3Error(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	var sb strings.Builder
	sb.WriteString(`<DeleteResult>`)
	for _, target := range doc.Objects {
		kept := b.Objects[:0]
		for _, o := range b.Objects {
			if o.Key == target.Key && (target.VersionID == "" || o.VersionID == target.VersionID) {
				continue
			}
			kept = append(kept, o)
		}
		b.Objects = kept
		fmt.Fprintf(&sb, `<Deleted><Key>%s</Key><VersionId>%s</VersionId></Deleted>`, target.Key, target.VersionID)
	}
	sb.WriteString(`</DeleteResult>`)
	writeXML(w, sb.String())
}
