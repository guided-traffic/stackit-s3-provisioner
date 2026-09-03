//go:build e2e

package e2e

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// TestBucketRBACAggregation verifies that the chart's user-facing ClusterRoles
// are aggregated into the built-in roles: a subject bound to view can list
// Buckets but neither create them nor write their status; a subject bound to
// edit can create and read Buckets but still cannot write their status.
//
// Aggregation is asynchronous — the controller-manager merges the labelled
// rules into view/edit some time after the chart install. Every allow is
// therefore polled, and every deny is checked only after the allow that
// proves aggregation has landed: before that, everything is denied and a
// deny-check would pass vacuously.
func TestBucketRBACAggregation(t *testing.T) {
	kube, _ := clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	const user = "e2e-rbac-subject"
	ns := createTestNamespace(ctx, t, kube, "e2e-rbac-")

	bindClusterRole(ctx, t, kube, ns, "view", user)
	requireEventuallyAllowed(ctx, t, kube, user, ns, "list", "buckets", "")
	requireDenied(ctx, t, kube, user, ns, "create", "buckets", "")
	requireDenied(ctx, t, kube, user, ns, "update", "buckets", "status")

	bindClusterRole(ctx, t, kube, ns, "edit", user)
	requireEventuallyAllowed(ctx, t, kube, user, ns, "create", "buckets", "")
	requireEventuallyAllowed(ctx, t, kube, user, ns, "get", "buckets", "")
	requireDenied(ctx, t, kube, user, ns, "update", "buckets", "status")
}

// createTestNamespace creates a namespace with the given name prefix and
// removes it when the test ends.
func createTestNamespace(ctx context.Context, t *testing.T, kube kubernetes.Interface, prefix string) string {
	t.Helper()
	ns, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: prefix},
	}, metav1.CreateOptions{})
	require.NoError(t, err, "create test namespace")
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_ = kube.CoreV1().Namespaces().Delete(cleanupCtx, ns.Name, metav1.DeleteOptions{})
	})
	return ns.Name
}

// bindClusterRole binds the named built-in ClusterRole to a synthetic user in
// the namespace. The RoleBinding goes away with the namespace.
func bindClusterRole(ctx context.Context, t *testing.T, kube kubernetes.Interface, ns, clusterRole, user string) {
	t.Helper()
	_, err := kube.RbacV1().RoleBindings(ns).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-" + clusterRole},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     clusterRole,
		},
		Subjects: []rbacv1.Subject{{
			APIGroup: rbacv1.GroupName,
			Kind:     rbacv1.UserKind,
			Name:     user,
		}},
	}, metav1.CreateOptions{})
	require.NoError(t, err, "bind ClusterRole %s to %s in %s", clusterRole, user, ns)
}

// canI asks the API server, via SubjectAccessReview, whether user may perform
// verb on the Bucket resource (optionally a subresource) in the namespace.
func canI(ctx context.Context, kube kubernetes.Interface, user, ns, verb, resource, subresource string) (bool, error) {
	sar := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User: user,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   ns,
				Verb:        verb,
				Group:       bucketGVR.Group,
				Resource:    resource,
				Subresource: subresource,
			},
		},
	}
	res, err := kube.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, err
	}
	return res.Status.Allowed, nil
}

// requireEventuallyAllowed polls until the access is granted, bounded by the
// test timeout, because aggregation lands asynchronously.
func requireEventuallyAllowed(ctx context.Context, t *testing.T, kube kubernetes.Interface, user, ns, verb, resource, subresource string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(ctx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
		allowed, err := canI(ctx, kube, user, ns, verb, resource, subresource)
		if err != nil {
			return false, nil
		}
		return allowed, nil
	})
	require.NoError(t, err, "%s should eventually be allowed to %s %s/%s in %s", user, verb, resource, subresource, ns)
}

// requireDenied checks once. Callers must have proven with a preceding
// requireEventuallyAllowed that aggregation has landed for the same binding,
// otherwise a denial says nothing.
func requireDenied(ctx context.Context, t *testing.T, kube kubernetes.Interface, user, ns, verb, resource, subresource string) {
	t.Helper()
	allowed, err := canI(ctx, kube, user, ns, verb, resource, subresource)
	require.NoError(t, err, "SubjectAccessReview")
	require.False(t, allowed, "%s must not be allowed to %s %s/%s in %s", user, verb, resource, subresource, ns)
}
