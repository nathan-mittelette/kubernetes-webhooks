package v1

import (
	"context"
	"encoding/json"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// newTestWebhook builds a DockerProxyMutatingWebhook wired with a real
// decoder, bypassing NewDockerProxyMutatingWebhook's YAML loading so tests
// can construct DockerConfig values directly.
func newTestWebhook(t *testing.T, config DockerConfig, pullSecret string) *DockerProxyMutatingWebhook {
	t.Helper()

	domainMapping, err := config.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	globalPullSecrets, _ := resolveGlobalPullSecrets(pullSecret, config.PullSecrets)
	if err := validateSecretNames(globalPullSecrets); err != nil {
		t.Fatalf("validateSecretNames: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	decoder := admission.NewDecoder(scheme)

	hook := &DockerProxyMutatingWebhook{
		config:            config,
		domainMapping:     domainMapping,
		globalPullSecrets: globalPullSecrets,
		PullSecret:        pullSecret,
	}
	if err := hook.InjectDecoder(&decoder); err != nil {
		t.Fatalf("InjectDecoder: %v", err)
	}
	return hook
}

func podAdmissionRequest(t *testing.T, namespace string, pod *corev1.Pod) (admission.Request, []byte) {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Namespace: namespace,
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
	return req, raw
}

// applyPatch applies the JSONPatch from an admission.Response to the
// original raw pod and returns the resulting Pod, so tests can assert on the
// final state rather than the raw patch operations.
func applyPatch(t *testing.T, original []byte, resp admission.Response) *corev1.Pod {
	t.Helper()
	if len(resp.Patches) == 0 {
		var pod corev1.Pod
		if err := json.Unmarshal(original, &pod); err != nil {
			t.Fatalf("unmarshal original: %v", err)
		}
		return &pod
	}

	patchJSON, err := json.Marshal(resp.Patches)
	if err != nil {
		t.Fatalf("marshal patches: %v", err)
	}
	patch, err := jsonpatch.DecodePatch(patchJSON)
	if err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	patched, err := patch.Apply(original)
	if err != nil {
		t.Fatalf("apply patch: %v", err)
	}
	var pod corev1.Pod
	if err := json.Unmarshal(patched, &pod); err != nil {
		t.Fatalf("unmarshal patched pod: %v", err)
	}
	return &pod
}

func secretNames(refs []corev1.LocalObjectReference) []string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.Name
	}
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHandlePullSecretMergeSemantics(t *testing.T) {
	baseConfig := func() DockerConfig {
		return DockerConfig{
			DomainMap: map[string]DomainMapping{
				"docker.io": {Target: "reg.ex.com/hub", PullSecrets: []string{"hub-creds"}},
				"gcr.io":    {Target: "other.ex.com", PullSecrets: []string{"gcr-creds"}},
			},
			PullSecrets: []string{"global-creds"},
		}
	}

	t.Run("rewrite + pod without secrets: global + used per-domain secrets added, in order", func(t *testing.T) {
		hook := newTestWebhook(t, baseConfig(), "")
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1.27"}}}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		if !resp.Allowed {
			t.Fatalf("expected the request to be allowed, got: %+v", resp)
		}
		result := applyPatch(t, raw, resp)

		if got := secretNames(result.Spec.ImagePullSecrets); !equalStrings(got, []string{"global-creds", "hub-creds"}) {
			t.Errorf("expected [global-creds hub-creds], got %v", got)
		}
	})

	t.Run("rewrite + pod with pre-existing secret: existing preserved, new ones appended", func(t *testing.T) {
		hook := newTestWebhook(t, baseConfig(), "")
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			Containers:       []corev1.Container{{Name: "app", Image: "nginx:1.27"}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "foo"}},
		}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		result := applyPatch(t, raw, resp)

		if got := secretNames(result.Spec.ImagePullSecrets); !equalStrings(got, []string{"foo", "global-creds", "hub-creds"}) {
			t.Errorf("expected [foo global-creds hub-creds] (existing preserved first), got %v", got)
		}
	})

	t.Run("pod already containing a configured secret: no duplicate", func(t *testing.T) {
		hook := newTestWebhook(t, baseConfig(), "")
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			Containers:       []corev1.Container{{Name: "app", Image: "nginx:1.27"}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "global-creds"}},
		}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		result := applyPatch(t, raw, resp)

		if got := secretNames(result.Spec.ImagePullSecrets); !equalStrings(got, []string{"global-creds", "hub-creds"}) {
			t.Errorf("expected no duplicate, got %v", got)
		}
	})

	t.Run("no rewrite: imagePullSecrets untouched", func(t *testing.T) {
		hook := newTestWebhook(t, baseConfig(), "")
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			Containers:       []corev1.Container{{Name: "app", Image: "unmapped-domain.com/x:1"}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "foo"}},
		}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		if resp.Patches != nil {
			result := applyPatch(t, raw, resp)
			if got := secretNames(result.Spec.ImagePullSecrets); !equalStrings(got, []string{"foo"}) {
				t.Errorf("expected imagePullSecrets untouched ([foo]), got %v", got)
			}
		}
	})

	t.Run("no secrets configured + rewrite: list untouched", func(t *testing.T) {
		hook := newTestWebhook(t, DockerConfig{DomainMap: map[string]DomainMapping{"docker.io": {Target: "reg.ex.com/hub"}}}, "")
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1.27"}}}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		result := applyPatch(t, raw, resp)
		if len(result.Spec.ImagePullSecrets) != 0 {
			t.Errorf("expected no imagePullSecrets, got %v", secretNames(result.Spec.ImagePullSecrets))
		}
	})

	t.Run("legacy replace bug regression: pre-existing secret must survive a rewrite", func(t *testing.T) {
		hook := newTestWebhook(t, DockerConfig{DomainMap: map[string]DomainMapping{"docker.io": {Target: "reg.ex.com/hub"}}}, "legacy-secret")
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			Containers:       []corev1.Container{{Name: "app", Image: "nginx:1.27"}},
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "private-registry-creds"}},
		}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		result := applyPatch(t, raw, resp)

		if got := secretNames(result.Spec.ImagePullSecrets); !equalStrings(got, []string{"private-registry-creds", "legacy-secret"}) {
			t.Errorf("expected [private-registry-creds legacy-secret], got %v", got)
		}
	})
}

func TestHandlePerDomainSelection(t *testing.T) {
	config := DockerConfig{
		DomainMap: map[string]DomainMapping{
			"docker.io": {Target: "reg.ex.com/hub", PullSecrets: []string{"hub-creds"}},
			"gcr.io":    {Target: "other.ex.com", PullSecrets: []string{"gcr-creds"}},
		},
	}

	t.Run("pod using only docker.io gets docker.io secrets, not gcr.io's", func(t *testing.T) {
		hook := newTestWebhook(t, config, "")
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1.27"}}}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		result := applyPatch(t, raw, resp)

		if got := secretNames(result.Spec.ImagePullSecrets); !equalStrings(got, []string{"hub-creds"}) {
			t.Errorf("expected [hub-creds], got %v", got)
		}
	})

	t.Run("pod mixing containers from two mapped domains gets the union", func(t *testing.T) {
		hook := newTestWebhook(t, config, "")
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "app", Image: "nginx:1.27"},
			{Name: "sidecar", Image: "gcr.io/foo/bar:v1"},
		}}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		result := applyPatch(t, raw, resp)

		if got := secretNames(result.Spec.ImagePullSecrets); !equalStrings(got, []string{"hub-creds", "gcr-creds"}) {
			t.Errorf("expected [hub-creds gcr-creds] (sorted by source domain: docker.io < gcr.io), got %v", got)
		}
	})

	t.Run("initContainer-only rewrite attaches that mapping's secrets", func(t *testing.T) {
		hook := newTestWebhook(t, config, "")
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "init", Image: "gcr.io/foo/bar:v1"}},
			Containers:     []corev1.Container{{Name: "app", Image: "unmapped-domain.com/x:1"}},
		}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		result := applyPatch(t, raw, resp)

		if got := secretNames(result.Spec.ImagePullSecrets); !equalStrings(got, []string{"gcr-creds"}) {
			t.Errorf("expected [gcr-creds], got %v", got)
		}
	})

	t.Run("already-conforming image performs no rewrite, so its mapping's secrets are not attached", func(t *testing.T) {
		hook := newTestWebhook(t, config, "")
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "reg.ex.com/hub/library/nginx:1.27"}}}}
		req, raw := podAdmissionRequest(t, "ns1", pod)

		resp := hook.Handle(context.Background(), req)
		if resp.Patches != nil {
			result := applyPatch(t, raw, resp)
			if len(result.Spec.ImagePullSecrets) != 0 {
				t.Errorf("expected no secrets attached for a no-op rewrite, got %v", secretNames(result.Spec.ImagePullSecrets))
			}
		}
	})
}

func TestHandlePullSecretsAddedMetric(t *testing.T) {
	config := DockerConfig{
		DomainMap:   map[string]DomainMapping{"docker.io": {Target: "reg.ex.com/hub"}},
		PullSecrets: []string{"global-creds"},
	}
	hook := newTestWebhook(t, config, "")
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx:1.27"}}}}
	req, _ := podAdmissionRequest(t, "metric_ns", pod)

	before := testutil.ToFloat64(pullSecretsAddedCounter.WithLabelValues("global-creds", "metric_ns"))
	hook.Handle(context.Background(), req)
	after := testutil.ToFloat64(pullSecretsAddedCounter.WithLabelValues("global-creds", "metric_ns"))

	if after != before+1 {
		t.Errorf("expected pull_secrets_added_total to increment by 1, went from %v to %v", before, after)
	}
}
