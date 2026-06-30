//go:build integration

package integration

import (
	"context"
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
	"github.com/guided-traffic/stackit-s3-provisioner/internal/controller"
)

// Package-level shared test infrastructure. A single envtest control plane and
// controller manager is started once and shared across all integration tests in
// this package, registering the Bucket controller exactly once.
var (
	testCtx    context.Context
	testCancel context.CancelFunc
	k8sClient  client.Client
	testEnv    *envtest.Environment
)

// TestMain sets up a shared envtest environment, registers schemes, starts the
// controller manager, and then runs all tests.
func TestMain(m *testing.M) {
	log.SetLogger(zap.New(zap.UseDevMode(true)))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("failed to start envtest: " + err.Error())
	}

	if err := s3v1.AddToScheme(scheme.Scheme); err != nil {
		panic("failed to register s3v1 scheme: " + err.Error())
	}
	if err := corev1.AddToScheme(scheme.Scheme); err != nil {
		panic("failed to register corev1 scheme: " + err.Error())
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic("failed to create manager: " + err.Error())
	}

	reconciler := &controller.BucketReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		OperatorVersion: "test",
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		panic("failed to setup controller: " + err.Error())
	}

	testCtx, testCancel = context.WithCancel(context.Background())

	go func() {
		if err := mgr.Start(testCtx); err != nil {
			panic("manager exited with error: " + err.Error())
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(testCtx) {
		panic("cache did not sync")
	}

	k8sClient = mgr.GetClient()

	code := m.Run()

	testCancel()
	if err := testEnv.Stop(); err != nil {
		panic("failed to stop envtest: " + err.Error())
	}

	os.Exit(code)
}
