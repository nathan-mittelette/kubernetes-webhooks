package v1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"sort"
	"strings"
)

type DockerProxyMutatingWebhook struct {
	Client     client.Client
	PullSecret string
	decoder    *admission.Decoder
	config     DockerConfig
	// domainMapping is the resolved, validated domainMap/ignoreList.
	domainMapping ResolvedDomainMapping
	// globalPullSecrets is the effective global secret list, after applying
	// legacy-flag/config precedence (see resolveGlobalPullSecrets).
	globalPullSecrets []string
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
	pullSecretsAddedCounter = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "docker_proxy_mutating_webhook_pull_secrets_added_total",
			Help: "Number of imagePullSecrets entries appended to pods",
		},
		[]string{"secret_name", "request_namespace"},
	)
)

// log is for logging in this package.
var log = logf.Log.WithName("docker-proxy-mutating-webhook")

func init() {
	metrics.Registry.MustRegister(webhookResultCounter, webhookFailureCounter, containerRewriteCounter, unknownDomainCounter, pullSecretsAddedCounter)
}

func NewDockerProxyMutatingWebhook(mutatingWebhookConfig []byte, client client.Client, pullSecret string) (*DockerProxyMutatingWebhook, error) {
	config := DockerConfig{}
	err := yaml.Unmarshal(mutatingWebhookConfig, &config)
	if err != nil {
		log.Error(err, "Unable to load config file.")
		return nil, err
	}

	if config.DomainMap == nil {
		err = errors.New("no domain mapping entries set")
		log.Error(err, "Invalid config.")
		return nil, err
	}
	log.Info("Domain mapping configuration loaded", "entries", len(config.DomainMap))

	if config.IgnoreList != nil {
		log.Info("Ignore list configuration loaded", "entries", len(config.IgnoreList))
		for _, ignore := range config.IgnoreList {
			log.Info("Ignore list entry", "value", ignore)
		}
	} else {
		log.Info("Ignore list empty")
	}

	domainMapping, err := config.resolve()
	if err != nil {
		log.Error(err, "Invalid domain mapping configuration.")
		return nil, err
	}

	globalPullSecrets, flagIgnored := resolveGlobalPullSecrets(pullSecret, config.PullSecrets)
	if err := validateSecretNames(globalPullSecrets); err != nil {
		err = fmt.Errorf("pullSecrets: %w", err)
		log.Error(err, "Invalid pull secret configuration.")
		return nil, err
	}
	if flagIgnored {
		log.Info("Both -pull-secret flag and config-level pullSecrets are set; config takes precedence", "ignoredFlagValue", pullSecret)
	}
	log.Info("Effective global pull secrets", "secrets", globalPullSecrets)

	return &DockerProxyMutatingWebhook{
		config:            config,
		domainMapping:     domainMapping,
		globalPullSecrets: globalPullSecrets,
		Client:            client,
		PullSecret:        pullSecret,
	}, nil
}

// ResolveSecretReplication resolves the loaded config's secretReplication
// section (see api/v1/config.go), reusing the already-parsed domain mapping
// and effective global pull secrets so the derived-secrets default reflects
// exactly what this webhook may attach to a pod.
func (webhook *DockerProxyMutatingWebhook) ResolveSecretReplication() (SecretReplicationSettings, error) {
	return webhook.config.resolveSecretReplication(webhook.globalPullSecrets, webhook.domainMapping)
}

// ResolveValidation resolves the loaded config's validation section (see
// api/v1/config.go), reusing the already-parsed domain mapping so
// autoAllowConfiguredDomains reflects exactly the domains this webhook maps
// or ignores — consistent domain matching between mutation and validation.
func (webhook *DockerProxyMutatingWebhook) ResolveValidation() (ResolvedValidation, error) {
	return webhook.config.resolveValidation(webhook.domainMapping)
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
	usedDomains := map[string]struct{}{}

	var containers []*corev1.Container
	for i := 0; i < len(pod.Spec.Containers); i++ {
		containers = append(containers, &pod.Spec.Containers[i])
	}
	for i := 0; i < len(pod.Spec.InitContainers); i++ {
		containers = append(containers, &pod.Spec.InitContainers[i])
	}

	for _, container := range containers {
		newImage, usedDomain, err := RewriteImage(container.Image, req.Namespace, webhook.domainMapping)
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
		if usedDomain != "" {
			usedDomains[usedDomain] = struct{}{}
		}
	}

	if changed {
		secretsToAttach := webhook.pullSecretsFor(usedDomains)
		appended := appendPullSecrets(pod, secretsToAttach)
		for _, name := range appended {
			pullSecretsAddedCounter.WithLabelValues(name, req.Namespace).Inc()
		}
		log.Info("Pull secrets evaluated", "attached", appended, "existingPreserved", true, "namespace", req.Namespace)
	}

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

// RewriteImage rewrites image according to mapping. The returned usedDomain
// is the domainMap source domain that was actually applied ("" if the image
// was already conforming, ignored, or unmapped) — callers use it to decide
// which per-domain pull secrets to attach, since secrets follow rewrites.
func RewriteImage(image string, namespace string, mapping ResolvedDomainMapping) (newImage string, usedDomain string, err error) {
	if anchoredShortIdentifierRegexp.MatchString(image) {
		// Do not process "identifiers"
		return image, "", nil
	}

	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		log.Error(err, "unable to parse image", "image", image)
		return "", "", err
	}

	domain := strings.ToLower(reference.Domain(named))
	path := reference.Path(named)

	// Already-conforming check: the image is already pointed at one of the
	// configured targets (and, if that target has a path prefix, the image
	// path is already under it) — return unchanged without touching any
	// metric. This makes rewriting idempotent, including under
	// reinvocationPolicy: IfNeeded.
	for _, target := range mapping.Targets {
		if target.Domain != domain {
			continue
		}
		if target.PathPrefix == "" || strings.HasPrefix(path, target.PathPrefix+"/") {
			return image, "", nil
		}
	}

	var newDomain string
	if _, ignored := mapping.IgnoreList[domain]; ignored {
		// ignoreList takes precedence over domainMap on overlap.
		newDomain = domain
	} else if target, ok := mapping.Targets[domain]; ok {
		log.Info("Domain mapped", "originalDomain", domain, "newDomain", target.Domain, "newPathPrefix", target.PathPrefix, "namespace", namespace)
		newDomain = target.String()
		usedDomain = domain
		containerRewriteCounter.WithLabelValues(domain, namespace).Inc()
	} else {
		log.Info("Found unmapped domain", "domain", domain, "namespace", namespace)
		unknownDomainCounter.WithLabelValues(domain, namespace).Inc()
		newDomain = domain
	}

	newImage = newDomain + "/" + path

	if t, ok := named.(reference.Tagged); ok {
		newImage += ":" + t.Tag()
	}

	if d, ok := named.(reference.Digested); ok {
		newImage += "@" + d.Digest().String()
	}

	return newImage, usedDomain, nil
}

// pullSecretsFor computes the deduplicated, deterministically ordered list of
// secrets to attach: global secrets first, then the secrets of every
// domainMap entry in usedDomains (sorted by source domain).
func (webhook *DockerProxyMutatingWebhook) pullSecretsFor(usedDomains map[string]struct{}) []string {
	seen := make(map[string]struct{})
	var result []string

	appendUnseen := func(name string) {
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}

	for _, name := range webhook.globalPullSecrets {
		appendUnseen(name)
	}

	domains := make([]string, 0, len(usedDomains))
	for domain := range usedDomains {
		domains = append(domains, domain)
	}
	sort.Strings(domains)

	for _, domain := range domains {
		for _, name := range webhook.domainMapping.Targets[domain].PullSecrets {
			appendUnseen(name)
		}
	}

	return result
}

// appendPullSecrets appends secrets to pod.Spec.ImagePullSecrets, preserving
// any existing entries (in their original position) and skipping names
// already present. It returns the names that were actually appended.
func appendPullSecrets(pod *corev1.Pod, secrets []string) []string {
	existing := make(map[string]struct{}, len(pod.Spec.ImagePullSecrets))
	for _, s := range pod.Spec.ImagePullSecrets {
		existing[s.Name] = struct{}{}
	}

	var appended []string
	for _, name := range secrets {
		if _, ok := existing[name]; ok {
			continue
		}
		pod.Spec.ImagePullSecrets = append(pod.Spec.ImagePullSecrets, corev1.LocalObjectReference{Name: name})
		existing[name] = struct{}{}
		appended = append(appended, name)
	}
	return appended
}

func (webhook *DockerProxyMutatingWebhook) InjectDecoder(decoder *admission.Decoder) error {
	webhook.decoder = decoder
	return nil
}
