// Package main is the entry point for the StackIT S3 provisioner operator.
package main

import (
	"encoding/json"
	"flag"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

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
	var bucketNamePrefix string
	var bucketNameIncludeNamespace bool
	var ownershipName string
	var enableWipeOnDelete bool
	var cloneImage string
	var driftResyncInterval time.Duration

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
	flag.StringVar(&bucketNamePrefix, "bucket-name-prefix", os.Getenv("BUCKET_NAME_PREFIX"),
		"Prefix prepended to every provisioned bucket name (e.g. a cluster id). Can also be set via "+
			"BUCKET_NAME_PREFIX. Empty disables the prefix. Must be a lowercase DNS-1123 label.")
	flag.BoolVar(&bucketNameIncludeNamespace, "bucket-name-include-namespace",
		envBoolOrDefault("BUCKET_NAME_INCLUDE_NAMESPACE", false),
		"Append the Bucket's namespace to the composed bucket name (after the prefix). "+
			"Can also be set via BUCKET_NAME_INCLUDE_NAMESPACE.")
	flag.StringVar(&ownershipName, "ownership-name",
		envOrDefault("OWNERSHIP_NAME", "stackit-s3-provisioner"),
		"Operator/fleet identity written into every provisioned bucket's managed-by tag "+
			"and required to match before the operator adopts or deletes a pre-existing bucket. "+
			"Can also be set via OWNERSHIP_NAME. WARNING: it is part of the bucket ownership key — "+
			"changing it after buckets exist makes the operator treat its own buckets as foreign. "+
			"On disaster-recovery restore into a new cluster, keep the same value.")
	flag.BoolVar(&enableWipeOnDelete, "enable-wipe-on-delete",
		envBoolOrDefault("ENABLE_WIPE_ON_DELETE", false),
		"Operator-wide feature gate for spec.wipeOnDelete: allow Buckets to request that all objects "+
			"are deleted before the bucket is removed on CR deletion. Can also be set via "+
			"ENABLE_WIPE_ON_DELETE. When disabled, a requested wipe degrades to the safe "+
			"empty-only delete guard and a warning event is emitted.")
	flag.StringVar(&cloneImage, "clone-image",
		envOrDefault("CLONE_IMAGE", controller.DefaultCloneImage),
		"Container image run by clone Jobs (spec.cloneFrom); an rclone image. "+
			"Can also be set via CLONE_IMAGE.")
	flag.DurationVar(&driftResyncInterval, "drift-resync-interval",
		envDurationOrDefault("DRIFT_RESYNC_INTERVAL", 10*time.Minute),
		"How often a provisioned Bucket is re-reconciled so configuration drift "+
			"(notably the isolation policy) self-heals without an event. The Bucket "+
			"watch only fires on generation/annotation changes, so a policy change "+
			"shipped in an operator upgrade otherwise never reaches already-provisioned "+
			"buckets. Can also be set via DRIFT_RESYNC_INTERVAL. Set to 0 to disable.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	naming := s3v1.BucketNaming{Prefix: bucketNamePrefix, IncludeNamespace: bucketNameIncludeNamespace}
	if err := naming.Validate(); err != nil {
		setupLog.Error(err, "invalid bucket naming configuration")
		os.Exit(1)
	}

	setupLog.Info("starting stackit-s3-provisioner",
		"version", version,
		"commit", commit,
		"buildTime", buildTime,
		"region", region,
		"bucketNamePrefix", bucketNamePrefix,
		"bucketNameIncludeNamespace", bucketNameIncludeNamespace,
		"ownershipName", ownershipName,
		"enableWipeOnDelete", enableWipeOnDelete,
		"driftResyncInterval", driftResyncInterval,
	)

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "stackit-s3-provisioner.stackit-bucket.gtrfc.com",
		// Release the lease immediately on graceful shutdown so the incoming pod
		// of a rolling update becomes leader within seconds instead of waiting out
		// the full lease duration. Safe because main() exits as soon as Start
		// returns (nothing runs after the manager stops).
		LeaderElectionReleaseOnCancel: true,
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

	// Clone Job pod resources are passed as a JSON-encoded
	// corev1.ResourceRequirements (Helm renders clone.resources into
	// CLONE_JOB_RESOURCES); empty applies none.
	var cloneResources corev1.ResourceRequirements
	if v := os.Getenv("CLONE_JOB_RESOURCES"); v != "" {
		if err := json.Unmarshal([]byte(v), &cloneResources); err != nil {
			setupLog.Error(err, "invalid CLONE_JOB_RESOURCES JSON")
			os.Exit(1)
		}
	}

	if err = (&controller.BucketReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Stackit:              stackitClient,
		OperatorVersion:      version,
		Naming:               naming,
		AdminSecretName:      adminSecretName,
		AdminSecretNamespace: operatorNamespace,
		OwnershipName:        ownershipName,
		EnableWipeOnDelete:   enableWipeOnDelete,
		CloneImage:           cloneImage,
		CloneJobResources:    cloneResources,
		DriftResyncInterval:  driftResyncInterval,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Bucket")
		os.Exit(1)
	}

	controller.RegisterBucketMetrics(mgr.GetClient(), stackitClient == nil, enableWipeOnDelete)

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

// envBoolOrDefault parses the environment variable key as a bool, returning def
// when it is unset or unparseable.
func envBoolOrDefault(key string, def bool) bool {
	if v, err := strconv.ParseBool(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// envDurationOrDefault parses the environment variable key as a Go duration
// (e.g. "10m"), returning def when it is unset or unparseable.
func envDurationOrDefault(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
