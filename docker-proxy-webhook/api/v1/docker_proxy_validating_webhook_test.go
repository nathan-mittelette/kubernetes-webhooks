package v1

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func boolPtr(b bool) *bool { return &b }

// TestMutationThenValidationAgree proves the ordering the whole feature
// depends on: Kubernetes runs mutating webhooks before validating ones, so
// validation must accept whatever RewriteImage produces. Both webhooks
// resolve their view of "allowed domains" from the same DockerConfig, via
// DockerProxyMutatingWebhook.Resolve{Validation,}, so they can never disagree.
func TestMutationThenValidationAgree(t *testing.T) {
	config := DockerConfig{
		DomainMap:  map[string]DomainMapping{"docker.io": {Target: "registry.example.com/hub"}},
		IgnoreList: []string{"private.example.com"},
		Validation: ValidationConfig{Enabled: true, Mode: ValidationModeEnforce},
	}

	mapping, err := config.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	validation, err := config.resolveValidation(mapping)
	if err != nil {
		t.Fatalf("resolveValidation: %v", err)
	}

	t.Run("a mutated image is accepted by validation", func(t *testing.T) {
		rewritten, _, err := RewriteImage("nginx:1.27", "ns1", mapping)
		if err != nil {
			t.Fatalf("RewriteImage: %v", err)
		}

		hook := newTestValidatingWebhook(t, validation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: rewritten}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatalf("expected the mutated image %q to be allowed by validation, got %+v", rewritten, resp)
		}
	})

	t.Run("an ignoreList domain passed through unchanged is accepted by validation", func(t *testing.T) {
		rewritten, _, err := RewriteImage("private.example.com/team/app:1", "ns1", mapping)
		if err != nil {
			t.Fatalf("RewriteImage: %v", err)
		}

		hook := newTestValidatingWebhook(t, validation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: rewritten}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatalf("expected the ignoreList image %q to be allowed by validation, got %+v", rewritten, resp)
		}
	})

	t.Run("an image that is never touched by domainMap/ignoreList is denied", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, validation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "evil.example.com/x:1"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if resp.Allowed {
			t.Fatalf("expected an unmapped, non-whitelisted image to be denied, got %+v", resp)
		}
	})
}

func TestResolveValidation(t *testing.T) {
	mapping := ResolvedDomainMapping{
		Targets: map[string]mappingTarget{
			"docker.io": {Domain: "reg.ex.com", PathPrefix: "hub"},
		},
		IgnoreList: map[string]struct{}{
			"private.ex.com": {},
		},
	}

	t.Run("section absent: disabled, no error", func(t *testing.T) {
		config := DockerConfig{}
		resolved, err := config.resolveValidation(mapping)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.Enabled {
			t.Errorf("expected disabled, got %+v", resolved)
		}
	})

	t.Run("mode defaults to enforce", func(t *testing.T) {
		config := DockerConfig{Validation: ValidationConfig{Enabled: true, AllowedDomains: []string{"registry.internal.example.com"}}}
		resolved, err := config.resolveValidation(mapping)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.Mode != ValidationModeEnforce {
			t.Errorf("expected mode %q, got %q", ValidationModeEnforce, resolved.Mode)
		}
	})

	t.Run("invalid mode rejected", func(t *testing.T) {
		config := DockerConfig{Validation: ValidationConfig{Enabled: true, Mode: "bogus", AllowedDomains: []string{"registry.internal.example.com"}}}
		if _, err := config.resolveValidation(mapping); err == nil {
			t.Fatal("expected an error for an invalid mode")
		}
	})

	t.Run("empty effective whitelist rejected", func(t *testing.T) {
		config := DockerConfig{Validation: ValidationConfig{Enabled: true, AutoAllowConfiguredDomains: boolPtr(false)}}
		if _, err := config.resolveValidation(mapping); err == nil {
			t.Fatal("expected an error for an empty effective whitelist")
		}
	})

	t.Run("autoAllowConfiguredDomains true by default merges domainMap + ignoreList", func(t *testing.T) {
		config := DockerConfig{Validation: ValidationConfig{Enabled: true}}
		resolved, err := config.resolveValidation(mapping)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := resolved.Whitelist["reg.ex.com"]; !ok {
			t.Errorf("expected domainMap target domain to be auto-allowed, got %+v", resolved.Whitelist)
		}
		if _, ok := resolved.Whitelist["private.ex.com"]; !ok {
			t.Errorf("expected ignoreList entry to be auto-allowed, got %+v", resolved.Whitelist)
		}
	})

	t.Run("autoAllowConfiguredDomains false honored", func(t *testing.T) {
		config := DockerConfig{Validation: ValidationConfig{
			Enabled:                    true,
			AutoAllowConfiguredDomains: boolPtr(false),
			AllowedDomains:             []string{"registry.internal.example.com"},
		}}
		resolved, err := config.resolveValidation(mapping)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := resolved.Whitelist["reg.ex.com"]; ok {
			t.Errorf("expected domainMap target domain to NOT be auto-allowed, got %+v", resolved.Whitelist)
		}
		if _, ok := resolved.Whitelist["registry.internal.example.com"]; !ok {
			t.Errorf("expected explicit allowedDomains entry to be present, got %+v", resolved.Whitelist)
		}
	})
}

func newTestValidatingWebhook(t *testing.T, validation ResolvedValidation) *DockerProxyValidatingWebhook {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	decoder := admission.NewDecoder(scheme)
	hook := NewDockerProxyValidatingWebhook(validation)
	if err := hook.InjectDecoder(&decoder); err != nil {
		t.Fatalf("InjectDecoder: %v", err)
	}
	return hook
}

func podValidationRequest(t *testing.T, namespace string, pod *corev1.Pod) admission.Request {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("marshal pod: %v", err)
	}
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "test-uid",
			Namespace: namespace,
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

func TestDockerProxyValidatingWebhookHandle(t *testing.T) {
	allowedValidation := ResolvedValidation{
		Enabled: true,
		Mode:    ValidationModeEnforce,
		Whitelist: map[string]struct{}{
			"registry.example.com": {},
			"docker.io":            {},
		},
	}

	t.Run("all containers allowed", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/foo:1"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatalf("expected allowed, got %+v", resp)
		}
	})

	t.Run("violation in containers denies with a message naming container and domain", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "evil", Image: "evil.example.com/x:1"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if resp.Allowed {
			t.Fatal("expected denial")
		}
		msg := resp.Result.Message
		if !strings.Contains(msg, "evil.example.com") || !strings.Contains(msg, "evil") {
			t.Errorf("expected message to name the domain and container, got %q", msg)
		}
	})

	t.Run("violation in initContainers denies", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: "init", Image: "evil.example.com/x:1"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if resp.Allowed {
			t.Fatal("expected denial for an initContainer violation")
		}
	})

	t.Run("violation in ephemeralContainers denies", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{
			Containers:          []corev1.Container{{Name: "app", Image: "docker.io/library/nginx:1.27"}},
			EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "evil.example.com/x:1"}}},
		}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if resp.Allowed {
			t.Fatal("expected denial for an ephemeralContainer violation (kubectl debug must not be a bypass)")
		}
	})

	t.Run("multiple violations are all listed", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "a", Image: "evil-a.example.com/x:1"},
			{Name: "b", Image: "evil-b.example.com/x:1"},
		}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		msg := resp.Result.Message
		if !strings.Contains(msg, "evil-a.example.com") || !strings.Contains(msg, "evil-b.example.com") {
			t.Errorf("expected both violations listed, got %q", msg)
		}
	})

	t.Run("warn mode allows with warnings", func(t *testing.T) {
		warnValidation := allowedValidation
		warnValidation.Mode = ValidationModeWarn
		hook := newTestValidatingWebhook(t, warnValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "evil", Image: "evil.example.com/x:1"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatal("expected allowed in warn mode")
		}
		if len(resp.Warnings) == 0 {
			t.Error("expected at least one warning to be attached")
		}
	})

	t.Run("disabled always allows", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, ResolvedValidation{})
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "evil", Image: "evil.example.com/x:1"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatal("expected disabled validation to always allow")
		}
	})

	t.Run("docker.io normalization: bare image name resolves to docker.io", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatalf("expected nginx (docker.io/library/nginx) to be allowed, got %+v", resp)
		}
	})

	t.Run("uppercase domain normalized before whitelist check", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "Registry.Example.COM/foo:1"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatalf("expected uppercase domain to normalize and be allowed, got %+v", resp)
		}
	})

	t.Run("image with digest allowed", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/foo@sha256:b494b781dbe0a164c7954a7ee9c9918ead58455b856045ae6d68c7c96192ac9d"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatalf("expected digest image to be allowed, got %+v", resp)
		}
	})

	t.Run("image with tag and digest allowed", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/foo:1@sha256:b494b781dbe0a164c7954a7ee9c9918ead58455b856045ae6d68c7c96192ac9d"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatalf("expected tag+digest image to be allowed, got %+v", resp)
		}
	})

	t.Run("short identifier skipped", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "4a581cd6feb1"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if !resp.Allowed {
			t.Fatalf("expected a short identifier to be skipped and allowed, got %+v", resp)
		}
	})

	t.Run("unparseable image denied", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "INVALID-UPPERCASE-REPO"}}}}
		resp := hook.Handle(context.Background(), podValidationRequest(t, "ns1", pod))
		if resp.Allowed {
			t.Fatal("expected an unparseable image to be denied")
		}
	})

	t.Run("wrong resource type errors", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Resource: metav1.GroupVersionResource{Version: "v1", Resource: "configmaps"},
		}}
		resp := hook.Handle(context.Background(), req)
		if resp.Result == nil || resp.Result.Code != 500 {
			t.Fatalf("expected an internal error response, got %+v", resp)
		}
	})

	t.Run("decode failure errors and counts failure metric", func(t *testing.T) {
		hook := newTestValidatingWebhook(t, allowedValidation)
		req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			Namespace: "decode_fail_ns",
			Resource:  metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
			Object:    runtime.RawExtension{Raw: []byte("not-json")},
		}}
		before := testutil.ToFloat64(validatingFailureCounter.WithLabelValues("decode_error", "decode_fail_ns"))
		resp := hook.Handle(context.Background(), req)
		if resp.Allowed {
			t.Fatal("expected a decode failure to be errored, not allowed")
		}
		after := testutil.ToFloat64(validatingFailureCounter.WithLabelValues("decode_error", "decode_fail_ns"))
		if after != before+1 {
			t.Errorf("expected failures_total to increment by 1, went from %v to %v", before, after)
		}
	})
}

func TestDockerProxyValidatingWebhookMetrics(t *testing.T) {
	validation := ResolvedValidation{
		Enabled:   true,
		Mode:      ValidationModeEnforce,
		Whitelist: map[string]struct{}{"registry.example.com": {}},
	}
	hook := newTestValidatingWebhook(t, validation)

	t.Run("result_total increments with the right allowed label", func(t *testing.T) {
		before := testutil.ToFloat64(validatingResultCounter.WithLabelValues("true", "metric_ns"))
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/foo:1"}}}}
		hook.Handle(context.Background(), podValidationRequest(t, "metric_ns", pod))
		after := testutil.ToFloat64(validatingResultCounter.WithLabelValues("true", "metric_ns"))
		if after != before+1 {
			t.Errorf("expected result_total{allowed=true} to increment by 1, went from %v to %v", before, after)
		}
	})

	t.Run("denied_images_total increments per rejected image", func(t *testing.T) {
		before := testutil.ToFloat64(validatingDeniedImagesCounter.WithLabelValues("evil.example.com", "metric_ns"))
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "evil.example.com/foo:1"}}}}
		hook.Handle(context.Background(), podValidationRequest(t, "metric_ns", pod))
		after := testutil.ToFloat64(validatingDeniedImagesCounter.WithLabelValues("evil.example.com", "metric_ns"))
		if after != before+1 {
			t.Errorf("expected denied_images_total to increment by 1, went from %v to %v", before, after)
		}
	})

	t.Run("warn mode also increments denied_images_total", func(t *testing.T) {
		warnValidation := validation
		warnValidation.Mode = ValidationModeWarn
		warnHook := newTestValidatingWebhook(t, warnValidation)

		before := testutil.ToFloat64(validatingDeniedImagesCounter.WithLabelValues("evil-warn.example.com", "metric_ns"))
		pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "evil-warn.example.com/foo:1"}}}}
		warnHook.Handle(context.Background(), podValidationRequest(t, "metric_ns", pod))
		after := testutil.ToFloat64(validatingDeniedImagesCounter.WithLabelValues("evil-warn.example.com", "metric_ns"))
		if after != before+1 {
			t.Errorf("expected denied_images_total to increment in warn mode too, went from %v to %v", before, after)
		}
	})
}
