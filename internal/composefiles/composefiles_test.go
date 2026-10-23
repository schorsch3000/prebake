// Package composefiles only holds a test: docker-compose.yaml and
// docker-compose.proxy.yaml are standalone files that must not drift apart.
package composefiles

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func load(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func dig(t *testing.T, m map[string]any, path ...string) map[string]any {
	t.Helper()
	for _, k := range path {
		next, ok := m[k].(map[string]any)
		if !ok {
			t.Fatalf("missing mapping %v", path)
		}
		m = next
	}
	return m
}

// The prebake service must be identical in both files except for the keys that are
// the whole point of the proxy variant.
func TestPrebakeServiceInSync(t *testing.T) {
	direct := dig(t, load(t, "../../docker-compose.yaml"), "services", "prebake")
	proxy := dig(t, load(t, "../../docker-compose.proxy.yaml"), "services", "prebake")

	strip := func(svc map[string]any) {
		delete(svc, "volumes")    // socket mount vs none
		delete(svc, "networks")   // proxy network
		delete(svc, "depends_on") // proxy health
		delete(dig(t, svc, "environment"), "DOCKER_HOST")
	}
	// the direct file must not mention the proxy variables at all
	if _, ok := dig(t, direct, "environment")["DOCKER_HOST"]; ok {
		t.Error("docker-compose.yaml must not set DOCKER_HOST")
	}
	strip(direct)
	strip(proxy)
	if !reflect.DeepEqual(direct, proxy) {
		t.Errorf("the prebake service differs between the two compose files beyond the proxy settings:\n direct: %v\n proxy:  %v", direct, proxy)
	}
}

func TestSocketMountOnlyInDirectFile(t *testing.T) {
	has := func(file string) bool {
		svc := dig(t, load(t, file), "services", "prebake")
		for _, v := range svc["volumes"].([]any) {
			if s, ok := v.(string); ok && strings.HasPrefix(s, "/var/run/docker.sock") {
				return true
			}
		}
		return false
	}
	if !has("../../docker-compose.yaml") {
		t.Error("docker-compose.yaml should mount the Docker socket")
	}
	if has("../../docker-compose.proxy.yaml") {
		t.Error("docker-compose.proxy.yaml must not mount the Docker socket into prebake")
	}
}

func TestProxyFileSecurityBasics(t *testing.T) {
	proxy := load(t, "../../docker-compose.proxy.yaml")
	svc := dig(t, proxy, "services", "docker-proxy")
	if img, _ := svc["image"].(string); !strings.Contains(img, "@sha256:") {
		t.Errorf("the proxy image must be pinned by digest: %q", img)
	}
	if _, ok := svc["ports"]; ok {
		t.Error("the proxy must not publish ports")
	}
	if dig(t, proxy, "networks", "docker-api")["internal"] != true {
		t.Error("the docker-api network must be internal")
	}
	env := dig(t, svc, "environment")
	for _, k := range []string{"SP_ALLOW_GET", "SP_ALLOW_POST", "SP_ALLOWFROM"} {
		if env[k] == nil {
			t.Errorf("missing %s", k)
		}
	}
	// no allowlist entry may open the container API
	for _, k := range []string{"SP_ALLOW_HEAD", "SP_ALLOW_GET", "SP_ALLOW_POST", "SP_ALLOW_DELETE"} {
		if s, _ := env[k].(string); strings.Contains(s, "containers") || s == ".*" || s == "(.*)" {
			t.Errorf("%s allows the container API or everything: %s", k, s)
		}
	}
}
