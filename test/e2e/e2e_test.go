//go:build e2e

// Package e2e provides end-to-end smoke tests for the stackit-s3-provisioner.
// They run against a real Kubernetes cluster (typically Kind) with the operator
// deployed via Helm, and verify that the CRD is installed, the operator is
// healthy, and a Bucket custom resource is reconciled through the skeleton flow
// (finalizer added, Ready condition reported). They intentionally do NOT touch
// the real StackIT API — provisioning is gated behind a service-account key and
// is exercised by the integration tests in package stackit.
package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	operatorNamespace = "stackit-s3-provisioner-system"
	testTimeout       = 3 * time.Minute
	pollInterval      = 3 * time.Second
)

var bucketGVR = schema.GroupVersionResource{
	Group:    "stackit-bucket.gtrfc.com",
	Version:  "v1",
	Resource: "buckets",
}

func clients(t *testing.T) (kubernetes.Interface, dynamic.Interface) {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		kubeconfig = home + "/.kube/config"
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(t, err, "build kubeconfig")
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	return kube, dyn
}

// TestOperatorIsHealthy verifies the operator Deployment reports Available.
func TestOperatorIsHealthy(t *testing.T) {
	kube, _ := clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	err := wait.PollUntilContextTimeout(ctx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
		deps, err := kube.AppsV1().Deployments(operatorNamespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, nil
		}
		for i := range deps.Items {
			d := &deps.Items[i]
			if d.Status.AvailableReplicas >= 1 {
				return true, nil
			}
		}
		return false, nil
	})
	require.NoError(t, err, "operator deployment should become available in namespace %s", operatorNamespace)
}

// TestBucketSkeletonReconcile creates a Bucket and asserts the operator wires it
// through the skeleton reconcile path, then cleans it up.
func TestBucketSkeletonReconcile(t *testing.T) {
	_, dyn := clients(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	const name = "e2e-smoke"
	const namespace = "default"

	bucket := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "stackit-bucket.gtrfc.com/v1",
		"kind":       "Bucket",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"bucketName": "e2e-smoke-bucket",
			"secretRef":  map[string]any{"name": "e2e-smoke-s3"},
		},
	}}

	_, err := dyn.Resource(bucketGVR).Namespace(namespace).Create(ctx, bucket, metav1.CreateOptions{})
	require.NoError(t, err, "create Bucket CR")
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_ = dyn.Resource(bucketGVR).Namespace(namespace).Delete(cleanupCtx, name, metav1.DeleteOptions{})
		_ = wait.PollUntilContextTimeout(cleanupCtx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
			_, err := dyn.Resource(bucketGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
			return apierrors.IsNotFound(err), nil
		})
	})

	// Wait until the operator reports a Ready condition (skeleton: NotImplemented).
	err = wait.PollUntilContextTimeout(ctx, pollInterval, testTimeout, true, func(ctx context.Context) (bool, error) {
		got, err := dyn.Resource(bucketGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil
		}
		conditions, found, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
		if !found {
			return false, nil
		}
		for _, c := range conditions {
			cond, ok := c.(map[string]any)
			if ok && cond["type"] == "Ready" {
				return true, nil
			}
		}
		return false, nil
	})
	require.NoError(t, err, "operator should set a Ready condition on the Bucket")

	got, err := dyn.Resource(bucketGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	require.NoError(t, err)
	finalizers := got.GetFinalizers()
	assert.Contains(t, finalizers, "stackit-bucket.gtrfc.com/finalizer", "operator should add its finalizer")
}
