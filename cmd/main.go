// Package main is the entry point for the StackIT S3 provisioner operator.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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
	var saKeyReloadInterval time.Duration
	var region string
	var adminSecretName string
	var operatorNamespace string
	var bucketNamePrefix string
	var bucketNameIncludeNamespace bool
	var ownershipName string
	var enableWipeOnDelete bool
	var cloneImage string
	var driftResyncInterval time.Duration
	var providerDegradedGrace time.Duration
	var providerCircuitThreshold int
	var providerCircuitMaxCooldown time.Duration
	var usageEnabled bool
	var usageDefaultEnabled bool
	var usageInterval time.Duration
	var usageMinInterval time.Duration
	var usageMaxObjects int64
	var usageIncludeVersions bool
	var usageConcurrency int
	var usagePrice string
	var usageCurrency string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&saKeyPath, "stackit-sa-key-path", os.Getenv("STACKIT_SERVICE_ACCOUNT_KEY_PATH"),
		"Path to the StackIT service-account key JSON. Can also be set via STACKIT_SERVICE_ACCOUNT_KEY_PATH. "+
			"When empty the operator runs in skeleton mode and does not provision.")
	flag.DurationVar(&saKeyReloadInterval, "stackit-sa-key-reload-interval",
		envDurationOrDefault("STACKIT_SERVICE_ACCOUNT_KEY_RELOAD_INTERVAL", 30*time.Second),
		"How often the mounted service-account key file is re-read so a rotated key takes effect "+
			"without restarting the operator. This is the only input the operator picks up at "+
			"runtime; everything else takes effect at process start. A candidate key is put to work "+
			"only after it parses, names the same StackIT project, and mints a token in one live "+
			"call — a candidate that fails any of those is discarded and the running key is kept, "+
			"which is what stops a truncated write or a revoked key from taking down a healthy "+
			"operator. Can also be set via STACKIT_SERVICE_ACCOUNT_KEY_RELOAD_INTERVAL. Set to 0 to "+
			"disable the reload, which restores the restart-only behaviour exactly.")
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
	flag.DurationVar(&providerDegradedGrace, "provider-degraded-grace",
		envDurationOrDefault("PROVIDER_DEGRADED_GRACE", 30*time.Minute),
		"How long an already-provisioned Bucket keeps its Ready state while reconciles "+
			"fail for a non-definitive reason (unreachable StackIT API, gateway error page, "+
			"Kubernetes API blip). Ready reflects the last verified state of the bucket, not "+
			"the outcome of the last attempt to verify it, so a short provider outage no longer "+
			"marks every Bucket unhealthy at once. After the grace the Bucket drops to Failed as "+
			"before. A structured 401/403 from the API is definitive and never held. Set to 0 to "+
			"disable the hold. Can also be set via PROVIDER_DEGRADED_GRACE.")
	flag.IntVar(&providerCircuitThreshold, "provider-circuit-threshold",
		envIntOrDefault("PROVIDER_CIRCUIT_THRESHOLD", controller.DefaultCircuitThreshold),
		"How many Bucket reconciles must fail back to back, with no successful reconcile "+
			"in between, before the operator stops calling the StackIT API altogether and "+
			"holds every Bucket until a probe succeeds. An unbroken run of failures is what "+
			"distinguishes a provider outage from a single broken Bucket, whose failures are "+
			"interleaved with successes from the rest of the fleet. Holding costs a handful of "+
			"API calls per outage instead of one per retry per Bucket, and stops the operator "+
			"from driving a provider blip into an IP-level rate limit. Set to 0 to disable. "+
			"Can also be set via PROVIDER_CIRCUIT_THRESHOLD.")
	flag.DurationVar(&providerCircuitMaxCooldown, "provider-circuit-max-cooldown",
		envDurationOrDefault("PROVIDER_CIRCUIT_MAX_COOLDOWN", controller.DefaultCircuitMaxCooldown),
		"Upper bound on the wait between provider probes while the circuit is open, and "+
			"therefore on how long recovery lags behind the provider coming back. The wait "+
			"starts at 60s and doubles per failed probe up to this cap; 60s rather than less "+
			"so that a trip outlives a 30s scrape gap and stays visible to the alerting "+
			"rules that suppress themselves while the circuit is open. "+
			"Can also be set via PROVIDER_CIRCUIT_MAX_COOLDOWN.")
	flag.DurationVar(&driftResyncInterval, "drift-resync-interval",
		envDurationOrDefault("DRIFT_RESYNC_INTERVAL", 10*time.Minute),
		"How often a provisioned Bucket is re-reconciled so configuration drift "+
			"(notably the isolation policy) self-heals without an event. The Bucket "+
			"watch only fires on generation/annotation changes, so a policy change "+
			"shipped in an operator upgrade otherwise never reaches already-provisioned "+
			"buckets. Can also be set via DRIFT_RESYNC_INTERVAL. Set to 0 to disable.")
	flag.BoolVar(&usageEnabled, "bucket-usage-enabled",
		envBoolOrDefault("BUCKET_USAGE_ENABLED", true),
		"Operator-wide gate for periodic bucket size measurement. Set to false to stop all "+
			"measurement traffic at once: no bucket is measured, whatever its CR asks for, and a "+
			"Bucket that asked explicitly gets a warning event. Can also be set via BUCKET_USAGE_ENABLED.")
	flag.BoolVar(&usageDefaultEnabled, "bucket-usage-default-enabled",
		envBoolOrDefault("BUCKET_USAGE_DEFAULT_ENABLED", false),
		"Whether Buckets that do not set spec.usage.enabled themselves are measured. This is the "+
			"cluster-wide default; a Bucket can override it in either direction as long as the gate "+
			"above is on. Can also be set via BUCKET_USAGE_DEFAULT_ENABLED.")
	flag.DurationVar(&usageInterval, "bucket-usage-interval",
		envDurationOrDefault("BUCKET_USAGE_INTERVAL", controller.DefaultUsageInterval),
		"How often a Bucket that does not request its own interval is measured. Must be a Go "+
			"duration string WITH A UNIT (e.g. \"1h\"). Can also be set via BUCKET_USAGE_INTERVAL.")
	flag.DurationVar(&usageMinInterval, "bucket-usage-min-interval",
		envDurationOrDefault("BUCKET_USAGE_MIN_INTERVAL", controller.DefaultUsageMinInterval),
		"Lower bound for spec.usage.interval; a Bucket asking for less is clamped up to it and told "+
			"so in status. Measuring means listing the bucket end to end (about one request per 1000 "+
			"object keys), so this floor is what stops a single CR from turning the operator into a "+
			"listing loop. Can also be set via BUCKET_USAGE_MIN_INTERVAL.")
	flag.Int64Var(&usageMaxObjects, "bucket-usage-max-objects",
		envInt64OrDefault("BUCKET_USAGE_MAX_OBJECTS", controller.DefaultUsageMaxObjects),
		"Maximum number of listing entries one measurement consumes before it stops and reports a "+
			"LOWER BOUND (status.usage.truncated). It bounds how long a single pass can take on a "+
			"bucket with very many objects. 0 removes the cap. Can also be set via BUCKET_USAGE_MAX_OBJECTS.")
	flag.BoolVar(&usageIncludeVersions, "bucket-usage-include-versions",
		envBoolOrDefault("BUCKET_USAGE_INCLUDE_VERSIONS", false),
		"Whether non-current object versions and delete markers are counted by default. They occupy "+
			"billed storage but are invisible in a plain object listing, so counting them makes the "+
			"measurement match the invoice on a versioned bucket at the price of a more expensive "+
			"listing. Can also be set via BUCKET_USAGE_INCLUDE_VERSIONS.")
	flag.IntVar(&usageConcurrency, "bucket-usage-concurrency",
		envIntOrDefault("BUCKET_USAGE_CONCURRENCY", controller.DefaultUsageConcurrency),
		"How many buckets are measured in parallel. Measurement runs in its own controller, so this "+
			"bounds listing traffic without ever delaying provisioning. Can also be set via "+
			"BUCKET_USAGE_CONCURRENCY.")
	flag.StringVar(&usagePrice, "bucket-usage-price-per-gb-hour",
		os.Getenv("BUCKET_USAGE_PRICE_PER_GB_HOUR"),
		"Price of one gigabyte for one hour, in whole currency units, used to estimate each Bucket's "+
			"monthly storage cost (e.g. \"0.00003697772\"). Empty or 0 disables the estimate. The "+
			"estimate prices the measured size for a 720-hour month and is not an invoice. Can also be "+
			"set via BUCKET_USAGE_PRICE_PER_GB_HOUR.")
	flag.StringVar(&usageCurrency, "bucket-usage-currency",
		envOrDefault("BUCKET_USAGE_CURRENCY", controller.DefaultUsageCurrency),
		"Currency the cost estimate is labelled with. Display only; no conversion happens. "+
			"Can also be set via BUCKET_USAGE_CURRENCY.")

	// Development mode picks the console encoder and a debug default level. The
	// Helm chart overrides the level through LOGLEVEL (logging.level, info by
	// default); the encoder stays console either way.
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if err := applyLogLevelEnv(flag.CommandLine); err != nil {
		// The logger is not built yet, so this goes where a flag parse error goes.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	naming := s3v1.BucketNaming{Prefix: bucketNamePrefix, IncludeNamespace: bucketNameIncludeNamespace}
	if err := naming.Validate(); err != nil {
		setupLog.Error(err, "invalid bucket naming configuration")
		os.Exit(1)
	}

	// The price is taken as a string and parsed here so a typo fails at startup
	// with a clear message instead of silently producing a cost of zero.
	pricePerGBHour, err := parseUsagePrice(usagePrice)
	if err != nil {
		setupLog.Error(err, "invalid bucket usage price", "value", usagePrice,
			"expected", "a non-negative decimal number of currency units per GB per hour")
		os.Exit(1)
	}
	usageConfig := controller.UsageConfig{
		Enabled:         usageEnabled,
		DefaultEnabled:  usageDefaultEnabled,
		Interval:        usageInterval,
		MinInterval:     usageMinInterval,
		MaxObjects:      usageMaxObjects,
		IncludeVersions: usageIncludeVersions,
		PricePerGBHour:  pricePerGBHour,
		Currency:        usageCurrency,
		Concurrency:     usageConcurrency,
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
		"providerDegradedGrace", providerDegradedGrace,
		"providerCircuitThreshold", providerCircuitThreshold,
		"providerCircuitMaxCooldown", providerCircuitMaxCooldown,
		"saKeyReloadInterval", saKeyReloadInterval,
		"bucketUsageEnabled", usageEnabled,
		"bucketUsageDefaultEnabled", usageDefaultEnabled,
		"bucketUsageInterval", usageInterval,
		"bucketUsageMinInterval", usageMinInterval,
		"bucketUsageMaxObjects", usageMaxObjects,
		"bucketUsagePricePerGBHour", pricePerGBHour,
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

	stackitClient, err := newStackitClient(saKeyPath, region, operatorNamespace)
	if err != nil {
		setupLog.Error(err, "unable to configure the StackIT client", "path", saKeyPath)
		os.Exit(1)
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

	providerBreaker := controller.NewProviderBreaker(providerCircuitThreshold, providerCircuitMaxCooldown)

	// The service-account key is the one input this operator re-reads at
	// runtime (ADR 0016 D1), because it is the one an external rotation
	// mechanism changes without anybody touching the deployment. Skeleton mode
	// and an interval of 0 both leave the poller off, and then none of the
	// sa_key series is exported at all — a mechanism that is not running must
	// not report a reassuring zero.
	if err := setupSAKeyReload(mgr, stackitClient, providerBreaker, saKeyPath, saKeyReloadInterval); err != nil {
		setupLog.Error(err, "unable to set up service-account key reloading")
		os.Exit(1)
	}

	if err = (&controller.BucketReconciler{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		Stackit:               stackitClient,
		OperatorVersion:       version,
		Naming:                naming,
		AdminSecretName:       adminSecretName,
		AdminSecretNamespace:  operatorNamespace,
		OwnershipName:         ownershipName,
		EnableWipeOnDelete:    enableWipeOnDelete,
		CloneImage:            cloneImage,
		CloneJobResources:     cloneResources,
		DriftResyncInterval:   driftResyncInterval,
		ProviderDegradedGrace: providerDegradedGrace,
		Breaker:               providerBreaker,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Bucket")
		os.Exit(1)
	}

	if err = (&controller.BucketUsageReconciler{
		Client:               mgr.GetClient(),
		Stackit:              stackitClient,
		Config:               usageConfig,
		AdminSecretName:      adminSecretName,
		AdminSecretNamespace: operatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BucketUsage")
		os.Exit(1)
	}

	controller.RegisterBucketMetrics(mgr.GetClient(), providerBreaker, stackitClient == nil, enableWipeOnDelete, usageEnabled)

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

// newStackitClient builds the provider client when a service-account key is
// configured. It returns a nil client and no error when none is: without a key
// the operator runs in skeleton mode and makes no cloud call at all
// (ADR 0005 D6). A key that is configured but unusable is a startup failure and
// never a silent fall back to skeleton mode (ADR 0005 D7).
func newStackitClient(saKeyPath, region, operatorNamespace string) (*stackit.Client, error) {
	if saKeyPath == "" {
		setupLog.Info("no StackIT service-account key configured; running in skeleton mode")
		return nil, nil
	}
	// Provisioning persists the bootstrap S3 admin credentials in a Secret in
	// the operator's own namespace, so that namespace must be known.
	if operatorNamespace == "" {
		return nil, errors.New("operator namespace unknown; set POD_NAMESPACE (or --operator-namespace) when a StackIT key is configured")
	}
	acc, err := stackit.LoadAccount(saKeyPath)
	if err != nil {
		return nil, fmt.Errorf("load StackIT service-account key: %w", err)
	}
	client, err := stackit.NewClient(acc, region)
	if err != nil {
		return nil, fmt.Errorf("build StackIT client: %w", err)
	}
	setupLog.Info("StackIT client configured", "project", acc.ProjectID, "region", region)
	return client, nil
}

// setupSAKeyReload registers the key poller with the manager, unless the
// operator has no key at all or the reload is switched off.
//
// The poller reports NeedLeaderElection() false, so it runs on every replica: a
// standby has to be holding a fresh client at the moment it takes the lease,
// not start reloading then (ADR 0016 D13).
func setupSAKeyReload(mgr ctrl.Manager, client *stackit.Client, breaker *controller.ProviderBreaker,
	keyPath string, interval time.Duration) error {
	if client == nil || interval <= 0 {
		return nil
	}
	observer := controller.NewSAKeyReloadObserver(
		ctrl.Log.WithName("sa-key-reload"), breaker, client.Account())
	controller.RegisterSAKeyReloadMetrics(observer)
	return mgr.Add(stackit.NewKeyReloader(client, keyPath, interval, observer.Observe))
}

// parseUsagePrice parses the configured price of one gigabyte for one hour. An
// empty value means "no price configured" and disables the cost estimate; a
// negative one is rejected rather than quietly producing negative costs.
func parseUsagePrice(raw string) (float64, error) {
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("price %q must not be negative", raw)
	}
	return v, nil
}

// applyLogLevelEnv makes LOGLEVEL the fallback for --zap-log-level. The flag is
// bound by controller-runtime, so unlike the operator's own flags it cannot read
// the variable as its default; the value is fed through the flag's own parser
// instead, so both surfaces accept exactly the same levels. An explicit flag
// wins, and the variable is then not even parsed.
func applyLogLevelEnv(fs *flag.FlagSet) error {
	v := os.Getenv("LOGLEVEL")
	if v == "" {
		return nil
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "zap-log-level" {
			explicit = true
		}
	})
	if explicit {
		return nil
	}
	if err := fs.Set("zap-log-level", v); err != nil {
		return fmt.Errorf("invalid value %q for LOGLEVEL: %w", v, err)
	}
	return nil
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

// envIntOrDefault parses the environment variable key as an int, returning def
// when it is unset or unparseable.
func envIntOrDefault(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

// envInt64OrDefault parses the environment variable key as an int64, returning
// def when it is unset or unparseable.
func envInt64OrDefault(key string, def int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil {
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
