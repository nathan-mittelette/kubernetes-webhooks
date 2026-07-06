package v1

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v2"
)

func TestParseMappingTarget(t *testing.T) {
	type tcase struct {
		name       string
		value      string
		wantDomain string
		wantPath   string
		wantErr    bool
	}

	tcases := []tcase{
		{name: "plain domain", value: "registry.example.com", wantDomain: "registry.example.com"},
		{name: "domain with port", value: "registry.example.com:5000", wantDomain: "registry.example.com:5000"},
		{name: "single-segment path prefix", value: "registry.example.com/docker-hub-remote", wantDomain: "registry.example.com", wantPath: "docker-hub-remote"},
		{name: "multi-segment path prefix", value: "registry.example.com/team/docker-hub-remote", wantDomain: "registry.example.com", wantPath: "team/docker-hub-remote"},
		{name: "uppercase normalized to lowercase", value: "Registry.Example.COM/Docker-Hub-Remote", wantDomain: "registry.example.com", wantPath: "docker-hub-remote"},
		{name: "bare docker.io target", value: "docker.io", wantDomain: "docker.io"},
		{name: "explicit docker.io with path", value: "docker.io/some-path", wantDomain: "docker.io", wantPath: "some-path"},
		{name: "localhost domain", value: "localhost/repo", wantDomain: "localhost", wantPath: "repo"},

		{name: "scheme prefix rejected", value: "https://registry.example.com", wantErr: true},
		{name: "trailing slash rejected", value: "registry.example.com/", wantErr: true},
		{name: "empty value rejected", value: "", wantErr: true},
		{name: "leading whitespace rejected", value: " registry.example.com", wantErr: true},
		{name: "trailing whitespace rejected", value: "registry.example.com ", wantErr: true},
		{name: "internal whitespace rejected", value: "registry.example.com/docker hub", wantErr: true},
		{name: "bare path without domain rejected", value: "myrepo", wantErr: true},
		{name: "bare path with slash without domain rejected", value: "myrepo/sub", wantErr: true},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := parseMappingTarget(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for value %q, got target %+v", tc.value, target)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for value %q: %v", tc.value, err)
			}
			if target.Domain != tc.wantDomain {
				t.Errorf("Domain: expected %q, got %q", tc.wantDomain, target.Domain)
			}
			if target.PathPrefix != tc.wantPath {
				t.Errorf("PathPrefix: expected %q, got %q", tc.wantPath, target.PathPrefix)
			}
		})
	}
}

func TestDomainMappingUnmarshalYAML(t *testing.T) {
	t.Run("string shorthand", func(t *testing.T) {
		var m DomainMapping
		if err := yaml.Unmarshal([]byte(`registry.example.com/quay-remote`), &m); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m.Target != "registry.example.com/quay-remote" || len(m.PullSecrets) != 0 {
			t.Errorf("unexpected result: %+v", m)
		}
	})

	t.Run("object form", func(t *testing.T) {
		var m DomainMapping
		doc := "target: registry.example.com/docker-hub-remote\npullSecrets:\n  - docker-hub-proxy-creds\n"
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := DomainMapping{Target: "registry.example.com/docker-hub-remote", PullSecrets: []string{"docker-hub-proxy-creds"}}
		if !reflect.DeepEqual(m, want) {
			t.Errorf("expected %+v, got %+v", want, m)
		}
	})

	t.Run("object form without target is rejected", func(t *testing.T) {
		var m DomainMapping
		doc := "pullSecrets:\n  - docker-hub-proxy-creds\n"
		if err := yaml.Unmarshal([]byte(doc), &m); err == nil {
			t.Fatalf("expected an error, got %+v", m)
		}
	})

	t.Run("mixed map of both forms", func(t *testing.T) {
		var config DockerConfig
		doc := `
domainMap:
  quay.io: registry.example.com/quay-remote
  docker.io:
    target: registry.example.com/docker-hub-remote
    pullSecrets:
      - docker-hub-proxy-creds
`
		if err := yaml.Unmarshal([]byte(doc), &config); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if config.DomainMap["quay.io"].Target != "registry.example.com/quay-remote" {
			t.Errorf("shorthand entry not parsed: %+v", config.DomainMap["quay.io"])
		}
		docker := config.DomainMap["docker.io"]
		if docker.Target != "registry.example.com/docker-hub-remote" || len(docker.PullSecrets) != 1 || docker.PullSecrets[0] != "docker-hub-proxy-creds" {
			t.Errorf("object entry not parsed: %+v", docker)
		}
	})
}

func TestValidateSecretNames(t *testing.T) {
	if err := validateSecretNames(nil); err != nil {
		t.Errorf("nil list should be valid: %v", err)
	}
	if err := validateSecretNames([]string{"docker-hub-proxy-creds", "other-creds"}); err != nil {
		t.Errorf("valid names should pass: %v", err)
	}
	if err := validateSecretNames([]string{"Invalid_Name!"}); err == nil {
		t.Error("expected an error for an invalid DNS-1123 subdomain")
	}
	if err := validateSecretNames([]string{"dup", "dup"}); err == nil {
		t.Error("expected an error for a duplicate secret name")
	}
}

func TestResolveGlobalPullSecrets(t *testing.T) {
	tcases := []struct {
		name            string
		flag            string
		config          []string
		wantSecrets     []string
		wantFlagIgnored bool
	}{
		{name: "neither set", flag: "", config: nil, wantSecrets: nil, wantFlagIgnored: false},
		{name: "flag only", flag: "legacy-secret", config: nil, wantSecrets: []string{"legacy-secret"}, wantFlagIgnored: false},
		{name: "config only", flag: "", config: []string{"a", "b"}, wantSecrets: []string{"a", "b"}, wantFlagIgnored: false},
		{name: "both set: config wins, flag ignored", flag: "legacy-secret", config: []string{"a"}, wantSecrets: []string{"a"}, wantFlagIgnored: true},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			secrets, flagIgnored := resolveGlobalPullSecrets(tc.flag, tc.config)
			if !reflect.DeepEqual(secrets, tc.wantSecrets) {
				t.Errorf("secrets: expected %v, got %v", tc.wantSecrets, secrets)
			}
			if flagIgnored != tc.wantFlagIgnored {
				t.Errorf("flagIgnored: expected %v, got %v", tc.wantFlagIgnored, flagIgnored)
			}
		})
	}
}

func TestDockerConfigResolve(t *testing.T) {
	t.Run("valid mixed map", func(t *testing.T) {
		config := DockerConfig{
			IgnoreList: []string{"Private.Example.COM"},
			DomainMap: map[string]DomainMapping{
				"docker.io": {Target: "registry.example.com/docker-hub-remote", PullSecrets: []string{"docker-hub-proxy-creds"}},
				"quay.io":   {Target: "registry.example.com/quay-remote"},
				"gcr.io":    {Target: "other-registry.example.com"},
				"Foo.IO":    {Target: "registry.example.com/foo-remote"},
			},
		}

		mapping, err := config.resolve()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		docker := mapping.Targets["docker.io"]
		if docker.Domain != "registry.example.com" || docker.PathPrefix != "docker-hub-remote" {
			t.Errorf("docker.io: unexpected target %+v", docker)
		}
		if !reflect.DeepEqual(docker.PullSecrets, []string{"docker-hub-proxy-creds"}) {
			t.Errorf("docker.io: unexpected pull secrets %+v", docker.PullSecrets)
		}

		gcr := mapping.Targets["gcr.io"]
		if gcr.Domain != "other-registry.example.com" || gcr.PathPrefix != "" || len(gcr.PullSecrets) != 0 {
			t.Errorf("gcr.io: unexpected target %+v", gcr)
		}

		// Key normalization: uppercase key lowercased.
		if _, ok := mapping.Targets["foo.io"]; !ok {
			t.Errorf("expected uppercase key %q to be normalized to lowercase", "Foo.IO")
		}
		// ignoreList entries normalized to lowercase too.
		if _, ok := mapping.IgnoreList["private.example.com"]; !ok {
			t.Errorf("expected ignoreList entry to be normalized to lowercase")
		}
	})

	t.Run("invalid target value fails", func(t *testing.T) {
		config := DockerConfig{
			DomainMap: map[string]DomainMapping{"docker.io": {Target: "https://registry.example.com"}},
		}
		if _, err := config.resolve(); err == nil {
			t.Fatal("expected an error for a scheme-prefixed target value")
		}
	})

	t.Run("empty source domain fails", func(t *testing.T) {
		config := DockerConfig{
			DomainMap: map[string]DomainMapping{"": {Target: "registry.example.com"}},
		}
		if _, err := config.resolve(); err == nil {
			t.Fatal("expected an error for an empty source domain")
		}
	})

	t.Run("invalid per-domain secret name fails", func(t *testing.T) {
		config := DockerConfig{
			DomainMap: map[string]DomainMapping{"docker.io": {Target: "registry.example.com", PullSecrets: []string{"Invalid_Name!"}}},
		}
		if _, err := config.resolve(); err == nil {
			t.Fatal("expected an error for an invalid pull secret name")
		}
	})
}

func TestResolveSecretReplication(t *testing.T) {
	mapping := ResolvedDomainMapping{
		Targets: map[string]mappingTarget{
			"docker.io": {Domain: "reg.ex.com", PathPrefix: "hub", PullSecrets: []string{"hub-creds"}},
			"gcr.io":    {Domain: "other.ex.com", PullSecrets: []string{"gcr-creds", "hub-creds"}},
		},
	}

	t.Run("disabled: no behavior change", func(t *testing.T) {
		config := DockerConfig{}
		settings, err := config.resolveSecretReplication([]string{"global-creds"}, mapping)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if settings.Enabled {
			t.Errorf("expected replication to be disabled by default, got %+v", settings)
		}
	})

	t.Run("enabled: derives the union of global + per-domain secrets", func(t *testing.T) {
		config := DockerConfig{SecretReplication: SecretReplicationConfig{Enabled: true}}
		settings, err := config.resolveSecretReplication([]string{"global-creds"}, mapping)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"gcr-creds", "global-creds", "hub-creds"}
		if !reflect.DeepEqual(settings.Secrets, want) {
			t.Errorf("expected %v, got %v", want, settings.Secrets)
		}
	})

	t.Run("enabled: explicit list overrides the derived default", func(t *testing.T) {
		config := DockerConfig{SecretReplication: SecretReplicationConfig{Enabled: true, Secrets: []string{"only-this-one"}}}
		settings, err := config.resolveSecretReplication([]string{"global-creds"}, mapping)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := []string{"only-this-one"}
		if !reflect.DeepEqual(settings.Secrets, want) {
			t.Errorf("expected %v, got %v", want, settings.Secrets)
		}
	})

	t.Run("enabled with empty resolved list is rejected", func(t *testing.T) {
		config := DockerConfig{SecretReplication: SecretReplicationConfig{Enabled: true}}
		if _, err := config.resolveSecretReplication(nil, ResolvedDomainMapping{}); err == nil {
			t.Fatal("expected an error when enabled with nothing to replicate")
		}
	})

	t.Run("enabled with an invalid explicit secret name is rejected", func(t *testing.T) {
		config := DockerConfig{SecretReplication: SecretReplicationConfig{Enabled: true, Secrets: []string{"Invalid_Name!"}}}
		if _, err := config.resolveSecretReplication(nil, mapping); err == nil {
			t.Fatal("expected an error for an invalid secret name")
		}
	})
}
