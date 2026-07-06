package controllers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestShouldSkipNamespace(t *testing.T) {
	now := metav1.Now()

	tcases := []struct {
		name string
		ns   corev1.Namespace
		want bool
	}{
		{
			name: "regular namespace is not skipped",
			ns:   corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}},
			want: false,
		},
		{
			name: "source namespace is skipped",
			ns:   corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "docker-proxy"}},
			want: true,
		},
		{
			name: "opted-out namespace is skipped",
			ns:   corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: map[string]string{DisabledLabelKey: DisabledLabelValue}}},
			want: true,
		},
		{
			name: "terminating namespace is skipped",
			ns:   corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-c", DeletionTimestamp: &now}},
			want: true,
		},
		{
			name: "unrelated label value does not opt out",
			ns:   corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-d", Labels: map[string]string{DisabledLabelKey: "something-else"}}},
			want: false,
		},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSkipNamespace(tc.ns, "docker-proxy"); got != tc.want {
				t.Errorf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

func TestIsManagedByUs(t *testing.T) {
	managed := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{ManagedByLabelKey: ManagedByLabelValue}}}
	if !isManagedByUs(managed) {
		t.Error("expected a secret with our managed-by label to be recognized as managed")
	}

	unmanaged := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "something-else"}}}
	if isManagedByUs(unmanaged) {
		t.Error("expected a secret without our managed-by label to not be recognized as managed")
	}

	noLabels := corev1.Secret{}
	if isManagedByUs(noLabels) {
		t.Error("expected a secret with no labels to not be recognized as managed")
	}
}

func TestNeedsUpdate(t *testing.T) {
	source := corev1.Secret{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "42"}}

	upToDate := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{SourceResourceVersionAnnotationKey: "42"}}}
	if needsUpdate(upToDate, source) {
		t.Error("expected a copy with a matching source resourceVersion to not need an update")
	}

	stale := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{SourceResourceVersionAnnotationKey: "41"}}}
	if !needsUpdate(stale, source) {
		t.Error("expected a copy with a stale source resourceVersion to need an update")
	}

	missingAnnotation := corev1.Secret{}
	if !needsUpdate(missingAnnotation, source) {
		t.Error("expected a copy with no recorded source resourceVersion to need an update")
	}
}

func TestBuildManagedCopy(t *testing.T) {
	source := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "docker-proxy-credentials", Namespace: "docker-proxy", ResourceVersion: "7"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{".dockerconfigjson": []byte("{}")},
	}

	copy := buildManagedCopy(source, "team-a")

	if copy.Name != source.Name || copy.Namespace != "team-a" {
		t.Errorf("unexpected identity: %s/%s", copy.Namespace, copy.Name)
	}
	if copy.Type != source.Type {
		t.Errorf("expected type %v, got %v", source.Type, copy.Type)
	}
	if string(copy.Data[".dockerconfigjson"]) != "{}" {
		t.Errorf("expected data to be copied, got %v", copy.Data)
	}
	if copy.Labels[ManagedByLabelKey] != ManagedByLabelValue {
		t.Errorf("expected managed-by label, got %v", copy.Labels)
	}
	if copy.Annotations[SourceAnnotationKey] != "docker-proxy/docker-proxy-credentials" {
		t.Errorf("expected source annotation, got %v", copy.Annotations[SourceAnnotationKey])
	}
	if copy.Annotations[SourceResourceVersionAnnotationKey] != "7" {
		t.Errorf("expected source resourceVersion annotation, got %v", copy.Annotations[SourceResourceVersionAnnotationKey])
	}
}
