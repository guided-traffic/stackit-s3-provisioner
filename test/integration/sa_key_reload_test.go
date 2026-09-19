//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

// TestSAKeyReloaderRunsOnAStandbyReplica pins ADR 0016 D13 against a real
// manager: the key poller must run on a replica that is NOT the leader, so a
// standby is already holding a fresh client at the moment it takes the lease
// rather than starting to reload then.
//
// The lease is deliberately taken by somebody else first and held for an hour,
// so this manager cannot become leader for the duration of the test. Without
// that, every runnable starts regardless and the test would prove nothing.
func TestSAKeyReloaderRunsOnAStandbyReplica(t *testing.T) {
	const leaseName = "sa-key-reload-standby-test"
	ctx, cancel := context.WithTimeout(testCtx, 60*time.Second)
	defer cancel()

	held := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: "default"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptr.To("another-replica"),
			LeaseDurationSeconds: ptr.To(int32(3600)),
			AcquireTime:          ptr.To(metav1.NowMicro()),
			RenewTime:            ptr.To(metav1.NowMicro()),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, held))
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), held) })

	mgr, err := ctrl.NewManager(testCfg, ctrl.Options{
		Scheme:                  scheme.Scheme,
		LeaderElection:          true,
		LeaderElectionID:        leaseName,
		LeaderElectionNamespace: "default",
		// Both servers off: the shared manager of this suite already holds the
		// default ports.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	require.NoError(t, err)

	// The key path does not exist, so every poll rejects. What is under test is
	// that the poll happens at all while this replica is not the leader.
	client, err := stackit.NewClientWithEndpoint(
		"11111111-2222-3333-4444-555555555555", stackit.RegionEU01, "http://127.0.0.1:1")
	require.NoError(t, err)

	polled := make(chan stackit.ReloadResult, 1)
	reloader := stackit.NewKeyReloader(client, filepath.Join(t.TempDir(), "absent.json"),
		50*time.Millisecond, func(res stackit.ReloadResult) {
			select {
			case polled <- res:
			default:
			}
		})
	require.NoError(t, mgr.Add(reloader))

	mgrCtx, stopMgr := context.WithCancel(ctx)
	stopped := make(chan error, 1)
	go func() { stopped <- mgr.Start(mgrCtx) }()
	t.Cleanup(func() {
		stopMgr()
		select {
		case err := <-stopped:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Error("manager did not stop after its context was cancelled")
		}
	})

	select {
	case res := <-polled:
		require.Equal(t, stackit.ReloadRejected, res.Outcome)
	case <-ctx.Done():
		t.Fatal("the key poller never ran on a manager that is not the leader")
	}

	// And it really was not the leader while that happened.
	select {
	case <-mgr.Elected():
		t.Fatal("the manager became leader although the lease is held elsewhere")
	default:
	}
}
