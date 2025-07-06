package main

import (
	"flag"
	v1 "github.com/nathan-mittelette/docker-proxy-webhook/api/v1"
	"os"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	// +kubebuilder:scaffold:imports
)

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
	flag.StringVar(&pullSecret, "pull-secret", "", "Include a pull secret in the pod configuration if the image reference has been rewritten. Leave empty to disable pull secrets.")
	flag.IntVar(&port, "listen-port", 9443, "The port the webhook endpoint binds to.")

	flag.Parse()

	// Always enable verbose logging for debugging
	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                server.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: healthAddr,
		WebhookServer:          webhook.NewServer(webhook.Options{Port: port}),
		LeaderElection:         false,
	})
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

	addMutatingWebhook(err, mgr, pullSecret)

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func addMutatingWebhook(err error, mgr manager.Manager, pullSecret string) {
	hookServer := mgr.GetWebhookServer()

	configPath := "/tmp/config/docker-proxy-config.yaml"
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		setupLog.Error(err, "Unable to read config file", "path", configPath)
		os.Exit(1)
	}

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
}
