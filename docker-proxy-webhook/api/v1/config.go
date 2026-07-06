package v1

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/util/validation"
)

// DockerConfig is the root of docker-proxy-config.yaml.
type DockerConfig struct {
	IgnoreList []string                 `yaml:"ignoreList"`
	DomainMap  map[string]DomainMapping `yaml:"domainMap"`
	// PullSecrets are appended to a pod's imagePullSecrets whenever any of
	// its container images was rewritten, regardless of which domainMap
	// entry matched.
	PullSecrets []string `yaml:"pullSecrets"`
	// SecretReplication configures automatic replication of pull secrets
	// into every namespace the webhook operates in.
	SecretReplication SecretReplicationConfig `yaml:"secretReplication"`
	// Validation configures the /validate registry-domain whitelist webhook.
	Validation ValidationConfig `yaml:"validation"`
}

// ValidationConfig is the "validation" section of docker-proxy-config.yaml.
type ValidationConfig struct {
	// Enabled is the master switch. When false (or the section is absent),
	// the /validate endpoint always allows — fully backward compatible.
	Enabled bool `yaml:"enabled"`
	// Mode is "enforce" (deny on violation, default) or "warn" (allow, but
	// attach an admission warning, log, and count a metric).
	Mode string `yaml:"mode"`
	// AutoAllowConfiguredDomains, when true (the default — nil means true),
	// implicitly whitelists every domainMap value and ignoreList entry so
	// the config does not have to be duplicated.
	AutoAllowConfiguredDomains *bool `yaml:"autoAllowConfiguredDomains"`
	// AllowedDomains lists additional explicitly allowed registry domains.
	AllowedDomains []string `yaml:"allowedDomains"`
}

const (
	ValidationModeEnforce = "enforce"
	ValidationModeWarn    = "warn"
)

// ResolvedValidation is the startup-validated, normalized form of
// ValidationConfig used at request time: a precomputed whitelist for O(1)
// lookup, no per-request allocation.
type ResolvedValidation struct {
	Enabled   bool
	Mode      string
	Whitelist map[string]struct{}
}

// resolveValidation validates and normalizes the validation section. When
// disabled (the default), it returns a zero-value, always-allow
// ResolvedValidation and no error — the config does not even need to be
// well-formed. AutoAllowConfiguredDomains folds mapping.Targets' domains and
// mapping.IgnoreList into the whitelist so the config isn't duplicated.
func (c DockerConfig) resolveValidation(mapping ResolvedDomainMapping) (ResolvedValidation, error) {
	if !c.Validation.Enabled {
		return ResolvedValidation{}, nil
	}

	mode := c.Validation.Mode
	if mode == "" {
		mode = ValidationModeEnforce
	}
	if mode != ValidationModeEnforce && mode != ValidationModeWarn {
		return ResolvedValidation{}, fmt.Errorf("validation.mode must be %q or %q, got %q", ValidationModeEnforce, ValidationModeWarn, mode)
	}

	autoAllow := true
	if c.Validation.AutoAllowConfiguredDomains != nil {
		autoAllow = *c.Validation.AutoAllowConfiguredDomains
	}

	whitelist := make(map[string]struct{}, len(c.Validation.AllowedDomains))
	for _, domain := range c.Validation.AllowedDomains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain != "" {
			whitelist[domain] = struct{}{}
		}
	}
	if autoAllow {
		for _, target := range mapping.Targets {
			whitelist[target.Domain] = struct{}{}
		}
		for domain := range mapping.IgnoreList {
			whitelist[domain] = struct{}{}
		}
	}

	if len(whitelist) == 0 {
		return ResolvedValidation{}, errors.New("validation is enabled but the effective allowed-domain whitelist is empty")
	}

	log.Info("Validation configuration resolved", "mode", mode, "autoAllowConfiguredDomains", autoAllow, "whitelistSize", len(whitelist))

	return ResolvedValidation{Enabled: true, Mode: mode, Whitelist: whitelist}, nil
}

// SecretReplicationConfig is the "secretReplication" section of
// docker-proxy-config.yaml.
type SecretReplicationConfig struct {
	// Enabled is the master switch. Default false — fully opt-in.
	Enabled bool `yaml:"enabled"`
	// Secrets, when non-empty, overrides the default derived list (every
	// secret referenced by pullSecrets / domainMap[].pullSecrets) with a
	// fixed list to replicate instead.
	Secrets []string `yaml:"secrets"`
}

// SecretReplicationSettings is the startup-resolved, validated form of
// SecretReplicationConfig, consumed by the SecretReplicationReconciler.
type SecretReplicationSettings struct {
	Enabled bool
	Secrets []string
}

// resolveSecretReplication resolves the secretReplication section: when no
// explicit secret list is configured, it defaults to the union of every
// secret referenced by the webhook's own configuration (global pullSecrets +
// every domainMap entry's pullSecrets). Fails fast if enabled with an empty
// resolved list (misconfiguration).
func (c DockerConfig) resolveSecretReplication(globalPullSecrets []string, mapping ResolvedDomainMapping) (SecretReplicationSettings, error) {
	if !c.SecretReplication.Enabled {
		return SecretReplicationSettings{}, nil
	}

	secrets := c.SecretReplication.Secrets
	if len(secrets) == 0 {
		secrets = derivedReplicationSecrets(globalPullSecrets, mapping)
	} else {
		secrets = dedupeSorted(secrets)
	}

	if err := validateSecretNames(secrets); err != nil {
		return SecretReplicationSettings{}, fmt.Errorf("secretReplication.secrets: %w", err)
	}
	if len(secrets) == 0 {
		return SecretReplicationSettings{}, errors.New("secretReplication is enabled but the resolved secret list is empty")
	}

	return SecretReplicationSettings{Enabled: true, Secrets: secrets}, nil
}

// derivedReplicationSecrets computes the default replication list: every
// secret the mutating webhook may attach, deduplicated and sorted.
func derivedReplicationSecrets(globalPullSecrets []string, mapping ResolvedDomainMapping) []string {
	all := append([]string(nil), globalPullSecrets...)
	for _, target := range mapping.Targets {
		all = append(all, target.PullSecrets...)
	}
	return dedupeSorted(all)
}

// dedupeSorted deduplicates names and returns them in a deterministic
// (sorted) order.
func dedupeSorted(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	result := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// DomainMapping is a domainMap entry. It accepts either the plain string
// shorthand ("proxy-domain[/path/prefix]", exactly today's syntax) or an
// object form carrying pull secrets specific to that mapping:
//
//	domainMap:
//	  quay.io: registry.example.com/quay-remote        # shorthand
//	  docker.io:                                        # object form
//	    target: registry.example.com/docker-hub-remote
//	    pullSecrets: [docker-hub-proxy-creds]
type DomainMapping struct {
	Target      string   `yaml:"target"`
	PullSecrets []string `yaml:"pullSecrets"`
}

// domainMappingPlain avoids infinite recursion when decoding the object form
// from within DomainMapping.UnmarshalYAML.
type domainMappingPlain DomainMapping

// UnmarshalYAML implements yaml.Unmarshaler: a scalar value is treated as the
// shorthand "target" string; a mapping is decoded as the full object form.
func (m *DomainMapping) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar string
	if err := unmarshal(&scalar); err == nil {
		m.Target = scalar
		m.PullSecrets = nil
		return nil
	}

	var full domainMappingPlain
	if err := unmarshal(&full); err != nil {
		return err
	}
	if full.Target == "" {
		return errors.New("domainMap entry object form requires a non-empty \"target\" field")
	}
	*m = DomainMapping(full)
	return nil
}

// mappingTarget is the parsed, validated form of a domainMap entry: the
// "proxy-domain[/path/prefix]" target plus any secrets specific to that
// mapping.
type mappingTarget struct {
	Domain      string   // "registry.example.com"
	PathPrefix  string   // "docker-hub-remote", or "" when the mapping has no path prefix
	PullSecrets []string // secrets attached only when this mapping is used
}

// String reconstructs the target as it should be rewritten into an image
// reference: "domain[/pathPrefix]".
func (t mappingTarget) String() string {
	if t.PathPrefix == "" {
		return t.Domain
	}
	return t.Domain + "/" + t.PathPrefix
}

// mappingProbeSuffix is appended to a candidate mapping value to build a
// synthetic reference that distribution/reference can validate, instead of
// hand-rolling a domain/path regex.
const mappingProbeSuffix = "probe:latest"

// parseMappingTarget validates and normalizes a domainMap value into a
// mappingTarget.
func parseMappingTarget(value string) (mappingTarget, error) {
	if value == "" {
		return mappingTarget{}, errors.New("mapping target is empty")
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return mappingTarget{}, fmt.Errorf("mapping target %q must not contain whitespace", value)
	}
	if strings.Contains(value, "://") {
		return mappingTarget{}, fmt.Errorf("mapping target %q must not include a scheme", value)
	}
	if strings.HasSuffix(value, "/") {
		return mappingTarget{}, fmt.Errorf("mapping target %q must not have a trailing slash", value)
	}

	lower := strings.ToLower(value)
	firstSlash := strings.Index(lower, "/")
	hasPath := firstSlash >= 0
	domainPart := lower
	pathPart := ""
	if hasPath {
		domainPart = lower[:firstSlash]
		pathPart = lower[firstSlash+1:]
	}

	// Validate the value as a registry host[/path] by parsing a synthetic
	// reference built from it, rather than hand-rolling a regex.
	named, err := reference.ParseNormalizedNamed(lower + "/" + mappingProbeSuffix)
	if err != nil {
		return mappingTarget{}, fmt.Errorf("mapping target %q is not a valid registry host/path: %w", value, err)
	}
	resolvedDomain := reference.Domain(named)

	// distribution/reference silently treats a bare, dot-less, non-localhost,
	// non-explicit-"docker.io" first segment as a path on Docker Hub rather
	// than as a domain (e.g. "myrepo/probe" normalizes to domain "docker.io").
	// Surface that ambiguity as a validation error instead of silently
	// mapping to docker.io.
	if resolvedDomain == "docker.io" && domainPart != "docker.io" && domainPart != "index.docker.io" {
		return mappingTarget{}, fmt.Errorf("mapping target %q does not look like a registry domain (expected host[.tld][:port][/path])", value)
	}

	if !hasPath {
		return mappingTarget{Domain: resolvedDomain}, nil
	}
	return mappingTarget{Domain: resolvedDomain, PathPrefix: pathPart}, nil
}

// ResolvedDomainMapping is the startup-validated, normalized form of
// DockerConfig's domainMap/ignoreList used at request time: lowercased
// source domains mapped to parsed+validated targets, and a lowercased
// ignore set.
type ResolvedDomainMapping struct {
	Targets    map[string]mappingTarget
	IgnoreList map[string]struct{}
}

// resolve validates and normalizes the config into a ResolvedDomainMapping,
// failing fast on malformed domainMap values (consistent with the existing
// startup-validation style).
func (c DockerConfig) resolve() (ResolvedDomainMapping, error) {
	targets := make(map[string]mappingTarget, len(c.DomainMap))
	for from, entry := range c.DomainMap {
		fromLower := strings.ToLower(strings.TrimSpace(from))
		if fromLower == "" {
			return ResolvedDomainMapping{}, errors.New("domainMap contains an empty source domain")
		}
		target, err := parseMappingTarget(entry.Target)
		if err != nil {
			return ResolvedDomainMapping{}, fmt.Errorf("domainMap[%q]: %w", from, err)
		}
		if err := validateSecretNames(entry.PullSecrets); err != nil {
			return ResolvedDomainMapping{}, fmt.Errorf("domainMap[%q].pullSecrets: %w", from, err)
		}
		target.PullSecrets = append([]string(nil), entry.PullSecrets...)
		targets[fromLower] = target
	}

	ignoreList := make(map[string]struct{}, len(c.IgnoreList))
	for _, entry := range c.IgnoreList {
		ignoreList[strings.ToLower(strings.TrimSpace(entry))] = struct{}{}
	}

	for from, target := range targets {
		log.Info("Domain mapping resolved", "from", from, "toDomain", target.Domain, "toPath", target.PathPrefix, "pullSecrets", target.PullSecrets)
		if _, overlap := ignoreList[from]; overlap {
			log.Info("domainMap entry is also present in ignoreList; ignoreList takes precedence", "domain", from)
		}
		if _, sourceIsAlsoTarget := targets[target.Domain]; sourceIsAlsoTarget {
			log.Info("domainMap target domain is also used as a source domain; verify this is intentional to avoid double-rewriting", "domain", target.Domain)
		}
	}

	return ResolvedDomainMapping{Targets: targets, IgnoreList: ignoreList}, nil
}

// validateSecretNames rejects invalid Kubernetes secret names (must be a
// valid DNS-1123 subdomain, the same rule the API server applies) and
// duplicate entries within the same list.
func validateSecretNames(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			return fmt.Errorf("invalid secret name %q: %s", name, strings.Join(errs, "; "))
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("duplicate secret name %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// resolveGlobalPullSecrets applies the legacy-flag/config precedence rule:
// non-empty config-level pullSecrets always win; the legacy -pull-secret flag
// value is used only as a fallback, as a single-element list, when the
// config declares none. flagIgnored reports whether a non-empty flag value
// was discarded in favor of the config, so the caller can log a warning.
func resolveGlobalPullSecrets(flagPullSecret string, configPullSecrets []string) (secrets []string, flagIgnored bool) {
	if len(configPullSecrets) > 0 {
		return configPullSecrets, flagPullSecret != ""
	}
	if flagPullSecret != "" {
		return []string{flagPullSecret}, false
	}
	return nil, false
}
