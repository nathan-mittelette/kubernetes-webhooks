package v1

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/distribution/reference"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"net/http"
	"regexp"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	"strings"
)

type DockerConfig struct {
	IgnoreList []string          `yaml:"ignoreList"`
	DomainMap  map[string]string `yaml:"domainMap"`
}

type DockerProxyMutatingWebhook struct {
	Client     client.Client
	PullSecret string
	decoder    *admission.Decoder
	config     DockerConfig
}

var (
	anchoredShortIdentifierRegexp = regexp.MustCompile("^[a-f0-9]{6,}$")

	webhookResultCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_mutating_webhook_result_total",
			Help: "Number of webhook invocations",
		},
		[]string{"mutated", "request_namespace"},
	)
	webhookFailureCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_mutating_webhook_failures_total",
			Help: "Number of webhook failures'",
		},
		[]string{"failure_reason", "request_namespace"},
	)
	containerRewriteCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_mutating_webhook_container_rewrites_total",
			Help: "Number of container image values rewritten",
		},
		[]string{"domain", "request_namespace"},
	)
	unknownDomainCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_mutating_webhook_unknown_domain_total",
			Help: "Number of unmapped domains",
		},
		[]string{"domain", "request_namespace"})
)

// log is for logging in this package.
var log = logf.Log.WithName("docker-proxy-mutating-webhook")

func init() {
	metrics.Registry.MustRegister(webhookResultCounter, webhookFailureCounter, containerRewriteCounter, unknownDomainCounter)
}

func NewDockerProxyMutatingWebhook(mutatingWebhookConfig []byte, client client.Client, pullSecret string) (*DockerProxyMutatingWebhook, error) {
	config := DockerConfig{}
	err := yaml.Unmarshal(mutatingWebhookConfig, &config)
	if err != nil {
		log.Error(err, "Unable to load config file.")
		return nil, err
	}

	if config.DomainMap != nil {
		log.Info("Domain mapping configuration loaded", "entries", len(config.DomainMap))
		for from, to := range config.DomainMap {
			log.Info("Remapping entry", "from", from, "to", to)
		}
	} else {
		err = errors.New("no domain mapping entries set")
		log.Error(err, "Invalid config.")
		return nil, err
	}

	if config.IgnoreList != nil {
		log.Info("Ignore list configuration loaded", "entries", len(config.IgnoreList))
		for _, ignore := range config.IgnoreList {
			log.Info("Ignore list entry", "value", ignore)
		}
	} else {
		log.Info("Ignore list empty")
	}

	log.Info("Pull secret startup configuration", "pullSecretConfigured", pullSecret != "", "pullSecretName", pullSecret)

	return &DockerProxyMutatingWebhook{config: config, Client: client, PullSecret: pullSecret}, nil
}

func (webhook *DockerProxyMutatingWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	log.Info("mutating pod", "namespace", req.Namespace, "name", req.Name, "uid", req.UID)

	if req.Resource.Resource != "pods" {
		webhookFailureCounter.WithLabelValues("invalid_resource_type", req.Namespace).Inc()

		err := errors.New("expect resource to be pods")
		logf.Log.Error(err, err.Error())
		return admission.Errored(http.StatusInternalServerError, err)
	}

	pod := &corev1.Pod{}

	if webhook.decoder == nil {
		err := errors.New("decoder not initialized")
		webhookFailureCounter.WithLabelValues("decoder_not_initialized", req.Namespace).Inc()
		log.Error(err, "Decoder is nil")
		return admission.Errored(http.StatusInternalServerError, err)
	}

	err := (*webhook.decoder).Decode(req, pod)
	if err != nil {
		webhookFailureCounter.WithLabelValues("decode_error", req.Namespace).Inc()

		log.Error(err, "failed to decode pod")
		return admission.Errored(http.StatusBadRequest, err)
	}

	changed := false

	var containers []*corev1.Container
	for i := 0; i < len(pod.Spec.Containers); i++ {
		containers = append(containers, &pod.Spec.Containers[i])
	}
	for i := 0; i < len(pod.Spec.InitContainers); i++ {
		containers = append(containers, &pod.Spec.InitContainers[i])
	}

	for _, container := range containers {
		newImage, err := RewriteImage(container.Image, req.Namespace, webhook.config)
		if err != nil {
			webhookFailureCounter.WithLabelValues("rewrite_failed", req.Namespace).Inc()

			log.Error(err, err.Error())
			return admission.Errored(http.StatusInternalServerError, err)
		}
		if newImage != container.Image {
			log.Info("Rewriting image", "oldImage", container.Image, "newImage", newImage, "namespace", req.Namespace, "containerName", container.Name)
			container.Image = newImage
			changed = true
		}
	}

	if changed {
		if webhook.PullSecret != "" {
			log.Info("Adding pull secret", "pullSecret", webhook.PullSecret, "namespace", req.Namespace)
			pod.Spec.ImagePullSecrets = []corev1.LocalObjectReference{
				{Name: webhook.PullSecret},
			}
		} else {
			log.Info("No pull secret configured - images rewritten without credentials", "namespace", req.Namespace)
		}
	}

	// Always log pullSecret configuration status for transparency
	log.Info("Pull secret configuration", "pullSecretConfigured", webhook.PullSecret != "", "pullSecretName", webhook.PullSecret, "namespace", req.Namespace)

	marshaledPod, err := json.Marshal(pod)
	if err != nil {
		webhookFailureCounter.WithLabelValues("marshaling_failed", req.Namespace).Inc()

		log.Error(err, err.Error())
		return admission.Errored(http.StatusInternalServerError, err)
	}

	if changed {
		log.Info("Pod images were rewritten", "namespace", req.Namespace, "name", req.Name)
		webhookResultCounter.WithLabelValues("true", req.Namespace).Inc()
		return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
	} else {
		log.Info("No pod images were rewritten", "namespace", req.Namespace, "name", req.Name)
		webhookResultCounter.WithLabelValues("false", req.Namespace).Inc()
		return admission.Allowed("No `image`s rewritten")
	}
}

func RewriteImage(image string, namespace string, config DockerConfig) (string, error) {
	if anchoredShortIdentifierRegexp.MatchString(image) {
		// Do not process "identifiers"
		return image, nil
	}

	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		log.Error(err, "unable to parse image", "image", image)
		return "", err
	}

	newImage := ""
	domain := strings.ToLower(reference.Domain(named))

	// Check if the domain matches a mapped domain value.
	// If so, it's already conforming & valid and does not need rewriting.
	for _, val := range config.DomainMap {
		if val == domain {
			return image, nil
		}
	}

	if val, ok := config.DomainMap[domain]; ok {
		log.Info("Domain mapped", "originalDomain", domain, "newDomain", val, "namespace", namespace)
		newImage = val
	}

	// Note: behaviour is unspecified if the domain appears in both the `DomainMap` and `IgnoreList`
	if config.IgnoreList != nil {
		for _, ignore := range config.IgnoreList {
			if domain == ignore {
				newImage = domain
				break
			}
		}
	}

	if newImage == "" {
		log.Info("Found unmapped domain", "domain", domain, "namespace", namespace)
		unknownDomainCounter.WithLabelValues(domain, namespace).Inc()
		newImage = domain
	} else {
		log.Info("Container image will be rewritten", "domain", domain, "namespace", namespace)
		containerRewriteCounter.WithLabelValues(domain, namespace).Inc()
	}

	newImage += "/" + reference.Path(named)

	if t, ok := named.(reference.Tagged); ok {
		newImage += ":" + t.Tag()
	}

	if d, ok := named.(reference.Digested); ok {
		newImage += "@" + d.Digest().String()
	}

	return newImage, nil
}

func (webhook *DockerProxyMutatingWebhook) InjectDecoder(decoder *admission.Decoder) error {
	webhook.decoder = decoder
	return nil
}
