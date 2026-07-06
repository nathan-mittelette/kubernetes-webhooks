package controllers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// envtestCfg is populated by TestMain if a real API server could be started.
// Tests that need it call requireEnvtest(t) first, which skips otherwise —
// this keeps `go test ./...` green without KUBEBUILDER_ASSETS configured,
// while `make test-envtest` (or CI with setup-envtest) exercises them for
// real. See docker-proxy-webhook/README.md's testing section.
var envtestCfg *rest.Config

func TestMain(m *testing.M) {
	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest environment unavailable, envtest-based tests will be skipped (%v)\n", err)
	} else {
		envtestCfg = cfg
	}

	code := m.Run()

	if envtestCfg != nil {
		_ = testEnv.Stop()
	}
	os.Exit(code)
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if envtestCfg == nil {
		t.Skip("envtest environment not available; set up KUBEBUILDER_ASSETS (e.g. `make envtest`) to run this test")
	}
}

// startReplicationManager starts a real manager running SecretReplicationReconciler
// against the shared envtest API server, and returns a client for test setup/assertions.
func startReplicationManager(t *testing.T, sourceNamespace string, secretNames []string) client.Client {
	t.Helper()

	mgr, err := ctrl.NewManager(envtestCfg, ctrl.Options{
		Scheme:                 scheme.Scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("unable to create manager: %v", err)
	}

	reconciler := &SecretReplicationReconciler{
		Client:          mgr.GetClient(),
		SourceNamespace: sourceNamespace,
		SecretNames:     secretNames,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		t.Fatalf("unable to set up reconciler: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() {
		if err := mgr.Start(ctx); err != nil {
			t.Logf("manager exited: %v", err)
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	return mgr.GetClient()
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !condition() {
		t.Fatal("condition was not met within timeout")
	}
}

func createNamespace(t *testing.T, c client.Client, name string, labels map[string]string) {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace %s: %v", name, err)
	}
}

func createSecret(t *testing.T, c client.Client, namespace, name string, data map[string][]byte, labels map[string]string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	if err := c.Create(context.Background(), secret); err != nil {
		t.Fatalf("create secret %s/%s: %v", namespace, name, err)
	}
	return secret
}

func getSecret(c client.Client, namespace, name string) (*corev1.Secret, error) {
	var secret corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, &secret)
	return &secret, err
}

// TestSecretReplicationAutomatic drives a single real manager through the
// end-to-end flows from feature 05 §6: seeding namespaces that already
// existed at startup, creation once the source secret exists, convergence on
// source rotation, drift repair, opt-out, and unmanaged-secret conflicts.
// Each scenario uses its own namespace so they don't interfere with each
// other. The reconciler tracks two secret names: "seed-secret" (created
// before the manager starts, to prove startup seeding) and the main
// "docker-proxy-credentials" (deliberately left uncreated until the first
// scenario, to also exercise the source-missing failure path).
func TestSecretReplicationAutomatic(t *testing.T) {
	requireEnvtest(t)

	const sourceNamespace = "src-auto"
	const secretName = "docker-proxy-credentials"
	const seedSecretName = "seed-secret"
	const seedTargetNamespace = "auto-target-0-seeded-at-startup"

	// Pre-create state before the controller (and therefore its informer
	// cache) even starts, so its first cache sync must pick these up on its
	// own — no separate seeding script.
	bootstrapClient, err := client.New(envtestCfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("unable to create bootstrap client: %v", err)
	}
	createNamespace(t, bootstrapClient, sourceNamespace, nil)
	createNamespace(t, bootstrapClient, seedTargetNamespace, nil)
	createSecret(t, bootstrapClient, sourceNamespace, seedSecretName, map[string][]byte{"k": []byte("seeded")}, nil)

	c := startReplicationManager(t, sourceNamespace, []string{secretName, seedSecretName})

	t.Run("namespaces that already existed before the controller started are seeded with no separate script", func(t *testing.T) {
		eventually(t, 5*time.Second, func() bool {
			copy, err := getSecret(c, seedTargetNamespace, seedSecretName)
			return err == nil && string(copy.Data["k"]) == "seeded" && isManagedByUs(*copy)
		})
	})

	t.Run("namespace created before the source secret exists fails with source_missing, then converges once it's created", func(t *testing.T) {
		const targetNamespace = "auto-target-1"
		before := testutil.ToFloat64(replicationFailures.WithLabelValues("source_missing", targetNamespace))

		createNamespace(t, c, targetNamespace, nil)

		eventually(t, 5*time.Second, func() bool {
			return testutil.ToFloat64(replicationFailures.WithLabelValues("source_missing", targetNamespace)) > before
		})

		createSecret(t, c, sourceNamespace, secretName, map[string][]byte{"k": []byte("v1")}, nil)

		eventually(t, 5*time.Second, func() bool {
			copy, err := getSecret(c, targetNamespace, secretName)
			return err == nil && string(copy.Data["k"]) == "v1" && isManagedByUs(*copy)
		})
	})

	t.Run("source rotation propagates automatically", func(t *testing.T) {
		const targetNamespace = "auto-target-2"
		createNamespace(t, c, targetNamespace, nil)

		eventually(t, 5*time.Second, func() bool {
			_, err := getSecret(c, targetNamespace, secretName)
			return err == nil
		})

		source, err := getSecret(c, sourceNamespace, secretName)
		if err != nil {
			t.Fatalf("get source secret: %v", err)
		}
		source.Data = map[string][]byte{"k": []byte("v2-rotated")}
		if err := c.Update(context.Background(), source); err != nil {
			t.Fatalf("update source secret: %v", err)
		}

		eventually(t, 5*time.Second, func() bool {
			copy, err := getSecret(c, targetNamespace, secretName)
			return err == nil && string(copy.Data["k"]) == "v2-rotated"
		})
	})

	t.Run("deleting a managed copy triggers drift repair", func(t *testing.T) {
		const targetNamespace = "auto-target-3"
		createNamespace(t, c, targetNamespace, nil)

		eventually(t, 5*time.Second, func() bool {
			_, err := getSecret(c, targetNamespace, secretName)
			return err == nil
		})

		copy, err := getSecret(c, targetNamespace, secretName)
		if err != nil {
			t.Fatalf("get copy: %v", err)
		}
		if err := c.Delete(context.Background(), copy); err != nil {
			t.Fatalf("delete copy: %v", err)
		}

		eventually(t, 5*time.Second, func() bool {
			_, err := getSecret(c, targetNamespace, secretName)
			return err == nil
		})
	})

	t.Run("opted-out namespace never receives a copy", func(t *testing.T) {
		const targetNamespace = "auto-target-4"
		createNamespace(t, c, targetNamespace, map[string]string{DisabledLabelKey: DisabledLabelValue})

		time.Sleep(500 * time.Millisecond)
		if _, err := getSecret(c, targetNamespace, secretName); !apierrors.IsNotFound(err) {
			t.Fatalf("expected no copy in an opted-out namespace, got err=%v", err)
		}
	})

	t.Run("opting out a namespace with existing managed copies stops management without deleting them", func(t *testing.T) {
		const targetNamespace = "auto-target-4b"
		createNamespace(t, c, targetNamespace, nil)

		eventually(t, 5*time.Second, func() bool {
			copy, err := getSecret(c, targetNamespace, secretName)
			return err == nil && string(copy.Data["k"]) == "v2-rotated" && isManagedByUs(*copy)
		})

		// Opt out. Existing copies must remain in place (not deleted).
		var ns corev1.Namespace
		if err := c.Get(context.Background(), types.NamespacedName{Name: targetNamespace}, &ns); err != nil {
			t.Fatalf("get namespace: %v", err)
		}
		if ns.Labels == nil {
			ns.Labels = map[string]string{}
		}
		ns.Labels[DisabledLabelKey] = DisabledLabelValue
		if err := c.Update(context.Background(), &ns); err != nil {
			t.Fatalf("update namespace: %v", err)
		}

		eventually(t, 5*time.Second, func() bool {
			var reconciled corev1.Namespace
			if err := c.Get(context.Background(), types.NamespacedName{Name: targetNamespace}, &reconciled); err != nil {
				return false
			}
			return reconciled.Labels[DisabledLabelKey] == DisabledLabelValue
		})

		// A subsequent source rotation must NOT propagate to the now-opted-out
		// namespace's copy: it stays stale rather than being deleted or updated.
		source, err := getSecret(c, sourceNamespace, secretName)
		if err != nil {
			t.Fatalf("get source secret: %v", err)
		}
		source.Data = map[string][]byte{"k": []byte("v3-after-optout")}
		if err := c.Update(context.Background(), source); err != nil {
			t.Fatalf("update source secret: %v", err)
		}

		// There is no event to wait for here (that's the point), so give the
		// controller a bounded window in which it would have reacted if it
		// were still managing this namespace, then assert it didn't.
		time.Sleep(2 * time.Second)

		stale, err := getSecret(c, targetNamespace, secretName)
		if err != nil {
			t.Fatalf("expected the existing copy to remain (not deleted) after opt-out: %v", err)
		}
		if string(stale.Data["k"]) != "v2-rotated" {
			t.Errorf("expected the opted-out namespace's copy to stay stale at %q, got %q", "v2-rotated", string(stale.Data["k"]))
		}
	})

	t.Run("pre-existing unmanaged secret is left untouched and counted as a conflict", func(t *testing.T) {
		const targetNamespace = "auto-target-5"
		// Create the namespace opted out first, so the controller does not
		// race to replicate into it before our own secret below exists.
		createNamespace(t, c, targetNamespace, map[string]string{DisabledLabelKey: DisabledLabelValue})
		createSecret(t, c, targetNamespace, secretName, map[string][]byte{"k": []byte("do-not-touch")}, map[string]string{"owner": "someone-else"})

		before := testutil.ToFloat64(replicationConflicts.WithLabelValues(targetNamespace))

		// Opt back in, which triggers a reconcile of this namespace.
		var ns corev1.Namespace
		if err := c.Get(context.Background(), types.NamespacedName{Name: targetNamespace}, &ns); err != nil {
			t.Fatalf("get namespace: %v", err)
		}
		delete(ns.Labels, DisabledLabelKey)
		if err := c.Update(context.Background(), &ns); err != nil {
			t.Fatalf("update namespace: %v", err)
		}

		eventually(t, 5*time.Second, func() bool {
			return testutil.ToFloat64(replicationConflicts.WithLabelValues(targetNamespace)) > before
		})

		unmanaged, err := getSecret(c, targetNamespace, secretName)
		if err != nil {
			t.Fatalf("get secret: %v", err)
		}
		if string(unmanaged.Data["k"]) != "do-not-touch" || isManagedByUs(*unmanaged) {
			t.Errorf("expected the unmanaged secret to be left untouched, got %+v", unmanaged)
		}
	})
}

// TestSecretReplicationGarbageCollection exercises the "secret removed from
// config" scenario directly via Reconcile: this mirrors what happens after
// an operator removes a secret name from the config and restarts the
// deployment (a fresh reconciler with a shorter SecretNames list).
func TestSecretReplicationGarbageCollection(t *testing.T) {
	requireEnvtest(t)

	c, err := client.New(envtestCfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("unable to create client: %v", err)
	}

	const sourceNamespace = "src-gc"
	const targetNamespace = "gc-target"
	createNamespace(t, c, sourceNamespace, nil)
	createNamespace(t, c, targetNamespace, nil)
	createSecret(t, c, sourceNamespace, "creds-a", map[string][]byte{"k": []byte("a")}, nil)
	createSecret(t, c, sourceNamespace, "creds-b", map[string][]byte{"k": []byte("b")}, nil)
	createSecret(t, c, targetNamespace, "unmanaged-other", map[string][]byte{"k": []byte("keep")}, map[string]string{"owner": "someone-else"})

	ctx := context.Background()

	reconcilerBoth := &SecretReplicationReconciler{Client: c, SourceNamespace: sourceNamespace, SecretNames: []string{"creds-a", "creds-b"}}
	if _, err := reconcilerBoth.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: targetNamespace}}); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}

	if _, err := getSecret(c, targetNamespace, "creds-a"); err != nil {
		t.Fatalf("expected creds-a to be replicated: %v", err)
	}
	if _, err := getSecret(c, targetNamespace, "creds-b"); err != nil {
		t.Fatalf("expected creds-b to be replicated: %v", err)
	}

	// Simulate "creds-b removed from config" via a restart with a shorter list.
	reconcilerOne := &SecretReplicationReconciler{Client: c, SourceNamespace: sourceNamespace, SecretNames: []string{"creds-a"}}
	if _, err := reconcilerOne.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: targetNamespace}}); err != nil {
		t.Fatalf("gc reconcile: %v", err)
	}

	if _, err := getSecret(c, targetNamespace, "creds-a"); err != nil {
		t.Errorf("expected creds-a (still configured) to remain: %v", err)
	}
	if _, err := getSecret(c, targetNamespace, "creds-b"); !apierrors.IsNotFound(err) {
		t.Errorf("expected creds-b (no longer configured) to be garbage collected, got err=%v", err)
	}
	if _, err := getSecret(c, targetNamespace, "unmanaged-other"); err != nil {
		t.Errorf("expected the unmanaged secret to be untouched by garbage collection: %v", err)
	}
}
