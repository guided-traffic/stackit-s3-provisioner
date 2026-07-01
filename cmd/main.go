// Package main is the entry point for the StackIT S3 provisioner operator.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	s3v1 "github.com/guided-traffic/stackit-s3-provisioner/api/v1"
	"github.com/guided-traffic/stackit-s3-provisioner/internal/controller"
	"github.com/guided-traffic/stackit-s3-provisioner/stackit"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	// Build information, set via ldflags.
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(s3v1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var saKeyPath string
	var region string
	var adminSecretName string
	var operatorNamespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&saKeyPath, "stackit-sa-key-path", os.Getenv("STACKIT_SERVICE_ACCOUNT_KEY_PATH"),
		"Path to the StackIT service-account key JSON. Can also be set via STACKIT_SERVICE_ACCOUNT_KEY_PATH. "+
			"When empty the operator runs in skeleton mode and does not provision.")
	flag.StringVar(&region, "stackit-region", envOrDefault("STACKIT_REGION", stackit.RegionEU01),
		"StackIT region the operator provisions in. Can also be set via STACKIT_REGION.")
	flag.StringVar(&adminSecretName, "admin-credentials-secret-name",
		envOrDefault("ADMIN_CREDENTIALS_SECRET_NAME", "stackit-s3-provisioner-admin"),
		"Name of the operator-owned Secret holding the bootstrap S3 admin credentials used to set bucket policies.")
	flag.StringVar(&operatorNamespace, "operator-namespace", os.Getenv("POD_NAMESPACE"),
		"Namespace the operator runs in; used to store the bootstrap S3 admin credentials Secret. "+
			"Defaults to POD_NAMESPACE.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	setupLog.Info("starting stackit-s3-provisioner",
		"version", version,
		"commit", commit,
		"buildTime", buildTime,
		"region", region,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "stackit-s3-provisioner.stackit-bucket.gtrfc.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Build the StackIT client when a service-account key is configured. Without
	// it, the operator runs in skeleton mode (no cloud calls).
	var stackitClient *stackit.Client
	if saKeyPath != "" {
		acc, err := stackit.LoadAccount(saKeyPath)
		if err != nil {
			setupLog.Error(err, "unable to load StackIT service-account key", "path", saKeyPath)
			os.Exit(1)
		}
		stackitClient, err = stackit.NewClient(acc, region)
		if err != nil {
			setupLog.Error(err, "unable to build StackIT client")
			os.Exit(1)
		}
		// Provisioning persists the bootstrap S3 admin credentials in a Secret in
		// the operator's own namespace, so that namespace must be known.
		if operatorNamespace == "" {
			setupLog.Error(nil, "operator namespace unknown; set POD_NAMESPACE (or --operator-namespace) when a StackIT key is configured")
			os.Exit(1)
		}
		setupLog.Info("StackIT client configured", "project", acc.ProjectID, "region", region)
	} else {
		setupLog.Info("no StackIT service-account key configured; running in skeleton mode")
	}

	if err = (&controller.BucketReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Stackit:              stackitClient,
		OperatorVersion:      version,
		AdminSecretName:      adminSecretName,
		AdminSecretNamespace: operatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Bucket")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// envOrDefault returns the value of the environment variable key, or def when unset.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
