/*
Copyright 2020 NEXT Trucking.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package v1

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// domainMapFromStrings builds a map[string]DomainMapping from the plain
// string-shorthand values used throughout these tests.
func domainMapFromStrings(m map[string]string) map[string]DomainMapping {
	out := make(map[string]DomainMapping, len(m))
	for k, v := range m {
		out[k] = DomainMapping{Target: v}
	}
	return out
}

func TestRewriteImage(t *testing.T) {
	type tcase struct {
		Image, Expected string
	}

	config := DockerConfig{
		IgnoreList: []string{"123456789012.dkr.ecr.us-east-1.amazonaws.com"},
		DomainMap: domainMapFromStrings(map[string]string{
			"docker.io":         "org-name-docker-io.jfrog.io",
			"quay.io":           "org-name-quay-io.jfrog.io",
			"gcr.io":            "org-name-gcr-io.jfrog.io",
			"k8s.gcr.io":        "org-name-k8s-gcr-io.jfrog.io",
			"docker.elastic.co": "org-name-docker-elastic-co.jfrog.io",
		}),
	}

	mapping, err := config.resolve()
	if err != nil {
		t.Fatalf("unexpected error resolving config: %v", err)
	}

	tcases := []tcase{
		{
			Image:    "123456789012.dkr.ecr.us-east-1.amazonaws.com/org-name/xyz-service:77092c522d97113ec952fce7d27cfec20be5fd82",
			Expected: "123456789012.dkr.ecr.us-east-1.amazonaws.com/org-name/xyz-service:77092c522d97113ec952fce7d27cfec20be5fd82",
		},
		{
			Image:    "prom/statsd-exporter:latest",
			Expected: "org-name-docker-io.jfrog.io/prom/statsd-exporter:latest",
		},
		{
			Image:    "vault:1.2.2",
			Expected: "org-name-docker-io.jfrog.io/library/vault:1.2.2",
		},
		{
			Image:    "docker:latest",
			Expected: "org-name-docker-io.jfrog.io/library/docker:latest",
		},
		{
			Image:    "quay.io/coreos/kube-state-metrics:v1.8.0",
			Expected: "org-name-quay-io.jfrog.io/coreos/kube-state-metrics:v1.8.0",
		},
		{
			Image:    "gcr.io/google_containers/metrics-server-amd64:v0.3.5",
			Expected: "org-name-gcr-io.jfrog.io/google_containers/metrics-server-amd64:v0.3.5",
		},
		{
			Image:    "k8s.gcr.io/defaultbackend-amd64:1.5",
			Expected: "org-name-k8s-gcr-io.jfrog.io/defaultbackend-amd64:1.5",
		},
		{
			Image:    "docker.elastic.co/beats/filebeat-oss:7.1.1",
			Expected: "org-name-docker-elastic-co.jfrog.io/beats/filebeat-oss:7.1.1",
		},
		{
			Image:    "unmapped-domain.com/registry/repo",
			Expected: "unmapped-domain.com/registry/repo",
		},
		{
			Image:    "org-name-docker-io.jfrog.io/already-mapped-domain/test",
			Expected: "org-name-docker-io.jfrog.io/already-mapped-domain/test",
		},
		{
			Image:    "kubernetes-ingress-controller/nginx-ingress-controller@sha256:b494b781dbe0a164c7954a7ee9c9918ead58455b856045ae6d68c7c96192ac9d",
			Expected: "org-name-docker-io.jfrog.io/kubernetes-ingress-controller/nginx-ingress-controller@sha256:b494b781dbe0a164c7954a7ee9c9918ead58455b856045ae6d68c7c96192ac9d",
		},
		{
			Image:    "clojure:openjdk-8-lein-2.9.1-alpine@sha256:d5454be246358c9cd683a06108ccdecc8b31a0341e45973af480bef708e8cf1e",
			Expected: "org-name-docker-io.jfrog.io/library/clojure:openjdk-8-lein-2.9.1-alpine@sha256:d5454be246358c9cd683a06108ccdecc8b31a0341e45973af480bef708e8cf1e",
		},
		{
			Image:    "4a581cd6feb1",
			Expected: "4a581cd6feb1",
		},
	}

	for _, tcase := range tcases {
		img, _, err := RewriteImage(tcase.Image, "my_namespace", mapping)
		if err != nil {
			t.Errorf("Expected: %v. Got error: %v", tcase.Expected, err)
		} else if img != tcase.Expected {
			t.Errorf("Expected: %v. Got: %v", tcase.Expected, img)
		}
	}

	// another test case for err != nil
	img, _, err := RewriteImage("INVALID-UPPERCASE-REPO", "my_namespace", mapping)
	if err == nil {
		t.Errorf("Expected error. Got: %v", img)
	}
}

func TestRewriteImagePathPrefix(t *testing.T) {
	const namespace = "path_prefix_ns"
	const digest = "sha256:b494b781dbe0a164c7954a7ee9c9918ead58455b856045ae6d68c7c96192ac9d"

	config := DockerConfig{
		IgnoreList: []string{"private.ex.com"},
		DomainMap: domainMapFromStrings(map[string]string{
			"docker.io": "reg.ex.com/hub",
			"quay.io":   "reg.ex.com/quay",
			"gcr.io":    "other.ex.com",
		}),
	}
	mapping, err := config.resolve()
	if err != nil {
		t.Fatalf("unexpected error resolving config: %v", err)
	}

	type tcase struct {
		name string
		// alwaysUnmapped marks cases whose domain is never a configured
		// source domain, so unlike mapped/conforming images they are not
		// expected to be reinvocation-idempotent on the unknown-domain
		// metric (this is the documented "same-host non-prefixed image"
		// decision from feature 03).
		alwaysUnmapped bool
		image          string
		expected       string
	}

	tcases := []tcase{
		{name: "path-prefix rewrite", image: "nginx:1.27", expected: "reg.ex.com/hub/library/nginx:1.27"},
		{name: "already conforming with prefix", image: "reg.ex.com/hub/library/nginx:1.27", expected: "reg.ex.com/hub/library/nginx:1.27"},
		{name: "same host, different prefix (quay)", image: "quay.io/x/y", expected: "reg.ex.com/quay/x/y"},
		{name: "same host, different prefix (hub)", image: "nginx", expected: "reg.ex.com/hub/library/nginx"},
		{name: "same-host non-prefixed image is unmapped", alwaysUnmapped: true, image: "reg.ex.com/random/img", expected: "reg.ex.com/random/img"},
		{name: "digest with prefix", image: "nginx@" + digest, expected: "reg.ex.com/hub/library/nginx@" + digest},
		{name: "tag+digest with prefix", image: "nginx:1.27@" + digest, expected: "reg.ex.com/hub/library/nginx:1.27@" + digest},
		{name: "second mapped hostname, no prefix", image: "gcr.io/foo/bar:v1", expected: "other.ex.com/foo/bar:v1"},
	}

	for _, tc := range tcases {
		t.Run(tc.name, func(t *testing.T) {
			img, _, err := RewriteImage(tc.image, namespace, mapping)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if img != tc.expected {
				t.Errorf("expected %q, got %q", tc.expected, img)
			}
		})
	}

	t.Run("idempotency: no unknown-domain metric on already-conforming image", func(t *testing.T) {
		before := testutil.ToFloat64(unknownDomainCounter.WithLabelValues("reg.ex.com", namespace))
		img, _, err := RewriteImage("reg.ex.com/hub/library/nginx:1.27", namespace, mapping)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if img != "reg.ex.com/hub/library/nginx:1.27" {
			t.Errorf("expected unchanged image, got %q", img)
		}
		after := testutil.ToFloat64(unknownDomainCounter.WithLabelValues("reg.ex.com", namespace))
		if after != before {
			t.Errorf("expected unknown_domain_total to stay at %v, got %v", before, after)
		}
	})

	t.Run("reinvocation is a no-op on every rewritten output", func(t *testing.T) {
		for _, tc := range tcases {
			first, _, err := RewriteImage(tc.image, namespace, mapping)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			domain := "reg.ex.com"
			if tc.name == "second mapped hostname, no prefix" {
				domain = "other.ex.com"
			}
			before := testutil.ToFloat64(unknownDomainCounter.WithLabelValues(domain, namespace))

			second, _, err := RewriteImage(first, namespace, mapping)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if second != first {
				t.Errorf("reinvocation on %q changed the image: %q -> %q", tc.image, first, second)
			}

			if !tc.alwaysUnmapped {
				after := testutil.ToFloat64(unknownDomainCounter.WithLabelValues(domain, namespace))
				if after != before {
					t.Errorf("reinvocation on %q incremented unknown_domain_total", first)
				}
			}
		}
	})

	t.Run("ignoreList takes precedence over domainMap on overlap", func(t *testing.T) {
		overlap := DockerConfig{
			IgnoreList: []string{"docker.io"},
			DomainMap:  domainMapFromStrings(map[string]string{"docker.io": "reg.ex.com/hub"}),
		}
		overlapMapping, err := overlap.resolve()
		if err != nil {
			t.Fatalf("unexpected error resolving config: %v", err)
		}
		img, _, err := RewriteImage("nginx:1.27", namespace, overlapMapping)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if img != "docker.io/library/nginx:1.27" {
			t.Errorf("expected ignoreList to win, got %q", img)
		}
	})
}
