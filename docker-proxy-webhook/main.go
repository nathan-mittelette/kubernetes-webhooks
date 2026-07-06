package main

import (
	"flag"
	"os"

	v1 "github.com/nathan-mittelette/docker-proxy-webhook/api/v1"
	"github.com/nathan-mittelette/docker-proxy-webhook/controllers"
	"gopkg.in/yaml.v2"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	// +kubebuilder:scaffold:imports
)

const configPath = "/tmp/config/docker-proxy-config.yaml"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)

	_ = corev1.AddToScheme(scheme)
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr, healthAddr, pullSecret string
	var port int

	flag.StringVar(&metricsAddr, "metrics-addr", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&healthAddr, "health-addr", ":8081", "The address the health endpoint binds to.")
	flag.StringVar(&pullSecret, "pull-secret", "", "[Deprecated, use the config file's global \"pullSecrets\" instead] Include a pull secret in the pod configuration if the image reference has been rewritten. Leave empty to disable pull secrets. Ignored (with a startup warning) if the config file sets any global pullSecrets.")
	flag.IntVar(&port, "listen-port", 9443, "The port the webhook endpoint binds to.")

	flag.Parse()

	// Always enable verbose logging for debugging
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		setupLog.Error(err, "Unable to read config file", "path", configPath)
		os.Exit(1)
	}

	podNamespace := os.Getenv("POD_NAMESPACE")

	managerOptions := ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: healthAddr,
		WebhookServer:          webhook.NewServer(webhook.Options{Port: port}),
		LeaderElection:         false,
	}
	if cacheOptions, ok := secretReplicationCacheOptions(configBytes, podNamespace); ok {
		managerOptions.Cache = cacheOptions
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Add readiness probe
	err = mgr.AddReadyzCheck("ready-ping", healthz.Ping)
	if err != nil {
		setupLog.Error(err, "unable add a readiness check")
		os.Exit(1)
	}

	// Add liveness probe
	err = mgr.AddHealthzCheck("health-ping", healthz.Ping)
	if err != nil {
		setupLog.Error(err, "unable add a health check")
		os.Exit(1)
	}

	hook := addMutatingWebhook(mgr, configBytes, pullSecret)
	addValidatingWebhook(mgr, hook)
	addSecretReplicationController(mgr, hook, podNamespace)

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func addMutatingWebhook(mgr manager.Manager, configBytes []byte, pullSecret string) *v1.DockerProxyMutatingWebhook {
	hookServer := mgr.GetWebhookServer()

	hook, err := v1.NewDockerProxyMutatingWebhook(configBytes, mgr.GetClient(), pullSecret)
	if err != nil {
		setupLog.Error(err, "Failed to create webhook")
		os.Exit(1)
	}

	// Create a decoder for the webhook
	decoder := admission.NewDecoder(scheme)
	err = hook.InjectDecoder(&decoder)
	if err != nil {
		setupLog.Error(err, "Failed to inject decoder")
		os.Exit(1)
	}

	hookServer.Register("/mutate", &webhook.Admission{Handler: hook})
	return hook
}

// addValidatingWebhook registers the /validate endpoint. It is always
// registered, even when validation.enabled is false in the config: in that
// case the handler always allows, so operators may apply the
// ValidatingWebhookConfiguration manifest unconditionally with no behavior
// change (see api/v1/docker_proxy_validating_webhook.go).
func addValidatingWebhook(mgr manager.Manager, hook *v1.DockerProxyMutatingWebhook) {
	settings, err := hook.ResolveValidation()
	if err != nil {
		setupLog.Error(err, "Invalid validation configuration")
		os.Exit(1)
	}
	setupLog.Info("Validation configuration", "enabled", settings.Enabled, "mode", settings.Mode, "whitelistSize", len(settings.Whitelist))

	validatingHook := v1.NewDockerProxyValidatingWebhook(settings)
	decoder := admission.NewDecoder(scheme)
	if err := validatingHook.InjectDecoder(&decoder); err != nil {
		setupLog.Error(err, "Failed to inject decoder into validating webhook")
		os.Exit(1)
	}

	mgr.GetWebhookServer().Register("/validate", &webhook.Admission{Handler: validatingHook})
}

// addSecretReplicationController registers the secret replication
// reconciler when secretReplication.enabled is true in the config. Fully
// opt-in: when disabled, no reconciler is registered and no additional RBAC
// is required.
func addSecretReplicationController(mgr manager.Manager, hook *v1.DockerProxyMutatingWebhook, podNamespace string) {
	settings, err := hook.ResolveSecretReplication()
	if err != nil {
		setupLog.Error(err, "Invalid secretReplication configuration")
		os.Exit(1)
	}
	if !settings.Enabled {
		setupLog.Info("Secret replication disabled")
		return
	}
	if podNamespace == "" {
		setupLog.Error(nil, "secretReplication is enabled but POD_NAMESPACE is not set (see manifests/secret-replication-rbac.yaml)")
		os.Exit(1)
	}

	setupLog.Info("Secret replication enabled", "sourceNamespace", podNamespace, "secrets", settings.Secrets)

	reconciler := &controllers.SecretReplicationReconciler{
		Client:          mgr.GetClient(),
		SourceNamespace: podNamespace,
		SecretNames:     settings.Secrets,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up secret replication controller")
		os.Exit(1)
	}
}

// secretReplicationCacheOptions peeks at the raw config to decide whether
// the manager's Secret cache must be scoped, before the manager (and
// therefore its cache) is built. When replication is enabled, the cache
// holds full Secret objects only in the source namespace and, everywhere
// else, only secrets carrying the managed-by label — the process never
// holds unrelated cluster secrets in memory. ok is false (no special
// options) when replication isn't enabled or the config can't be
// preparsed; NewDockerProxyMutatingWebhook/ResolveSecretReplication surface
// the authoritative error later.
func secretReplicationCacheOptions(configBytes []byte, podNamespace string) (cache.Options, bool) {
	var probe v1.DockerConfig
	if err := yaml.Unmarshal(configBytes, &probe); err != nil || !probe.SecretReplication.Enabled || podNamespace == "" {
		return cache.Options{}, false
	}

	managedSelector, err := labels.Parse(controllers.ManagedByLabelKey + "=" + controllers.ManagedByLabelValue)
	if err != nil {
		setupLog.Error(err, "unable to build the secret replication cache label selector")
		return cache.Options{}, false
	}

	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{
					podNamespace:        {},
					cache.AllNamespaces: {LabelSelector: managedSelector},
				},
			},
		},
	}, true
}
