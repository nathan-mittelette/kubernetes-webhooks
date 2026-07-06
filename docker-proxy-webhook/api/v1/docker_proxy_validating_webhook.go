package v1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var (
	validatingLog = logf.Log.WithName("docker-proxy-validating-webhook")

	validatingResultCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_validating_webhook_result_total",
			Help: "Every admission decision",
		},
		[]string{"allowed", "request_namespace"},
	)
	validatingDeniedImagesCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_validating_webhook_denied_images_total",
			Help: "Each rejected/warned image",
		},
		[]string{"domain", "request_namespace"},
	)
	validatingFailureCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_validating_webhook_failures_total",
			Help: "Decode/internal errors",
		},
		[]string{"failure_reason", "request_namespace"},
	)
)

func init() {
	metrics.Registry.MustRegister(validatingResultCounter, validatingDeniedImagesCounter, validatingFailureCounter)
}

// DockerProxyValidatingWebhook rejects (or warns about) pods whose container
// images are served from a registry domain outside the configured
// whitelist. It runs after the mutating webhook (Kubernetes admits all
// mutating webhooks before all validating ones), so it validates the
// post-rewrite image domain.
type DockerProxyValidatingWebhook struct {
	decoder    *admission.Decoder
	validation ResolvedValidation
}

// NewDockerProxyValidatingWebhook builds the validating webhook from an
// already-resolved whitelist (see DockerProxyMutatingWebhook.ResolveValidation,
// which shares the same parsed domainMap/ignoreList so mutation and
// validation agree on domain matching).
func NewDockerProxyValidatingWebhook(validation ResolvedValidation) *DockerProxyValidatingWebhook {
	return &DockerProxyValidatingWebhook{validation: validation}
}

func (webhook *DockerProxyValidatingWebhook) InjectDecoder(decoder *admission.Decoder) error {
	webhook.decoder = decoder
	return nil
}

// imageViolationMessage describes one offending (or unparseable) image.
type imageViolationMessage struct {
	domain  string // "" for a parse failure
	message string
}

func (webhook *DockerProxyValidatingWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	if !webhook.validation.Enabled {
		return admission.Allowed("validation disabled")
	}

	if req.Resource.Resource != "pods" {
		validatingFailureCounter.WithLabelValues("invalid_resource_type", req.Namespace).Inc()
		err := errors.New("expect resource to be pods (or the pods/ephemeralcontainers subresource)")
		validatingLog.Error(err, err.Error())
		return admission.Errored(http.StatusInternalServerError, err)
	}

	if webhook.decoder == nil {
		err := errors.New("decoder not initialized")
		validatingFailureCounter.WithLabelValues("decoder_not_initialized", req.Namespace).Inc()
		validatingLog.Error(err, "Decoder is nil")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	pod := &corev1.Pod{}
	if err := (*webhook.decoder).Decode(req, pod); err != nil {
		validatingFailureCounter.WithLabelValues("decode_error", req.Namespace).Inc()
		validatingLog.Error(err, "failed to decode pod")
		return admission.Errored(http.StatusBadRequest, err)
	}

	var violations []imageViolationMessage
	check := func(containerName, image string) {
		if v := webhook.checkImage(containerName, image); v != nil {
			violations = append(violations, *v)
		}
	}
	for _, c := range pod.Spec.Containers {
		check(c.Name, c.Image)
	}
	for _, c := range pod.Spec.InitContainers {
		check(c.Name, c.Image)
	}
	for _, c := range pod.Spec.EphemeralContainers {
		check(c.Name, c.Image)
	}

	allowed := len(violations) == 0
	validatingResultCounter.WithLabelValues(strconv.FormatBool(allowed), req.Namespace).Inc()

	if allowed {
		return admission.Allowed("all image domains whitelisted")
	}

	messages := make([]string, 0, len(violations))
	for _, v := range violations {
		validatingDeniedImagesCounter.WithLabelValues(v.domain, req.Namespace).Inc()
		messages = append(messages, v.message)
	}
	reason := strings.Join(messages, "; ")

	if webhook.validation.Mode == ValidationModeWarn {
		validatingLog.Info("Image domain violation (warn mode)", "namespace", req.Namespace, "name", req.Name, "violations", reason)
		return admission.Allowed("some image domains are not in the allowed registry list (warn mode)").WithWarnings(messages...)
	}

	validatingLog.Info("Denying pod: image domain violation", "namespace", req.Namespace, "name", req.Name, "violations", reason)
	return admission.Denied(reason)
}

// checkImage validates one container image against the whitelist, returning
// nil when it's allowed (or a short identifier, which is skipped exactly
// like the mutating webhook does).
func (webhook *DockerProxyValidatingWebhook) checkImage(containerName, image string) *imageViolationMessage {
	if anchoredShortIdentifierRegexp.MatchString(image) {
		return nil
	}

	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return &imageViolationMessage{
			domain:  "(unparseable)",
			message: fmt.Sprintf("image %q could not be parsed as a valid image reference (container %q)", image, containerName),
		}
	}

	domain := strings.ToLower(reference.Domain(named))
	if _, allowed := webhook.validation.Whitelist[domain]; allowed {
		return nil
	}

	return &imageViolationMessage{
		domain:  domain,
		message: fmt.Sprintf("image domain %q is not in the allowed registry list (container %q, image %q)", domain, containerName, image),
	}
}
