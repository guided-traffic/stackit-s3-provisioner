//go:build helm

// Package helm renders the chart with `helm template` and asserts on the
// user-facing RBAC objects. The Kind e2e install only ever exercises the
// default values, so the non-default combinations — and the exact shape of
// the roles, which the aggregation tests in test/e2e cannot see — are pinned
// here. Requires the helm binary on PATH; run via `make test-helm-render`.
package helm

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	chartDir    = "../../deploy/helm/stackit-s3-provisioner"
	releaseName = "rbac-test"
	// fullname is what the chart's fullname helper yields for releaseName:
	// the release name does not contain the chart name, so both are joined.
	fullname = releaseName + "-stackit-s3-provisioner"

	bucketGroup = "stackit-bucket.gtrfc.com"
)

type rule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

type object struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Rules []rule `json:"rules"`
}

// render runs `helm template` with the given --set arguments and indexes the
// rendered objects by kind/name.
func render(t *testing.T, sets ...string) map[string]object {
	t.Helper()
	args := make([]string, 0, 3+2*len(sets))
	args = append(args, "template", releaseName, chartDir)
	for _, s := range sets {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "helm template failed: %s", stderr.String())

	objs := map[string]object{}
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var o object
		err := dec.Decode(&o)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "decode rendered manifest")
		if o.Kind == "" { // empty document from a disabled template
			continue
		}
		objs[o.Kind+"/"+o.Metadata.Name] = o
	}
	return objs
}

// verbsFor returns the verbs a ClusterRole grants on one resource of the
// Bucket API group, or nil when the resource is not mentioned at all.
func verbsFor(o object, resource string) []string {
	var verbs []string
	for _, r := range o.Rules {
		if !contains(r.APIGroups, bucketGroup) || !contains(r.Resources, resource) {
			continue
		}
		verbs = append(verbs, r.Verbs...)
	}
	return verbs
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestDefaultsRenderAggregatedBucketRoles(t *testing.T) {
	objs := render(t)

	view, ok := objs["ClusterRole/"+fullname+"-view"]
	require.True(t, ok, "-view ClusterRole must be rendered by default")
	for _, l := range []string{"aggregate-to-view", "aggregate-to-edit", "aggregate-to-admin"} {
		assert.Equal(t, "true", view.Metadata.Labels["rbac.authorization.k8s.io/"+l], "-view label %s", l)
	}
	assert.ElementsMatch(t, []string{"get", "list", "watch"}, verbsFor(view, "buckets"))
	assert.ElementsMatch(t, []string{"get"}, verbsFor(view, "buckets/status"))
	assert.Nil(t, verbsFor(view, "buckets/finalizers"), "-view must not touch finalizers")

	edit, ok := objs["ClusterRole/"+fullname+"-edit"]
	require.True(t, ok, "-edit ClusterRole must be rendered by default")
	assert.Equal(t, "true", edit.Metadata.Labels["rbac.authorization.k8s.io/aggregate-to-edit"])
	assert.Equal(t, "true", edit.Metadata.Labels["rbac.authorization.k8s.io/aggregate-to-admin"])
	assert.Empty(t, edit.Metadata.Labels["rbac.authorization.k8s.io/aggregate-to-view"],
		"-edit must never aggregate into view")
	assert.ElementsMatch(t, []string{"create", "delete", "deletecollection", "patch", "update"},
		verbsFor(edit, "buckets"), "-edit is an aggregation fragment: write verbs only")
	assert.Nil(t, verbsFor(edit, "buckets/status"), "status is the operator's")
	assert.Nil(t, verbsFor(edit, "buckets/finalizers"), "-edit must not touch finalizers")

	requireOperatorRole(t, objs)
}

func TestCreateFalseRendersNoBucketRoles(t *testing.T) {
	objs := render(t, "bucketRoles.create=false")

	_, hasView := objs["ClusterRole/"+fullname+"-view"]
	_, hasEdit := objs["ClusterRole/"+fullname+"-edit"]
	assert.False(t, hasView, "-view must not be rendered with bucketRoles.create=false")
	assert.False(t, hasEdit, "-edit must not be rendered with bucketRoles.create=false")

	requireOperatorRole(t, objs)
}

// requireOperatorRole pins that the operator's own ClusterRole is untouched by
// the bucketRoles block: rendered, unlabelled, with its full Bucket verbs.
func requireOperatorRole(t *testing.T, objs map[string]object) {
	t.Helper()
	op, ok := objs["ClusterRole/"+fullname]
	require.True(t, ok, "operator ClusterRole must always be rendered")
	for k := range op.Metadata.Labels {
		assert.NotContains(t, k, "aggregate-to", "operator ClusterRole must not be aggregated")
	}
	assert.ElementsMatch(t, []string{"create", "delete", "get", "list", "patch", "update", "watch"},
		verbsFor(op, "buckets"))
	assert.ElementsMatch(t, []string{"get", "patch", "update"}, verbsFor(op, "buckets/status"))
	assert.ElementsMatch(t, []string{"update"}, verbsFor(op, "buckets/finalizers"))
}
