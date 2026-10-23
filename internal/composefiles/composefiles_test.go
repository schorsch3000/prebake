// Package composefiles only holds a test: docker-compose.yaml and
// docker-compose.proxy.yaml are standalone files that must not drift apart.
package composefiles

import (
	"os"
	"reflect"
	"regexp"
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

// The two deployment files use the published image and never build; building is the job of
// docker-compose.build.yaml, so `docker compose up` cannot silently build a stale local image.
func TestPublishedImageAndBuildOverride(t *testing.T) {
	for _, f := range []string{"../../docker-compose.yaml", "../../docker-compose.proxy.yaml"} {
		svc := dig(t, load(t, f), "services", "prebake")
		if _, ok := svc["build"]; ok {
			t.Errorf("%s: must not contain a build section (see docker-compose.build.yaml)", f)
		}
		img, _ := svc["image"].(string)
		if !strings.Contains(img, "PREBAKE_IMAGE") || !strings.Contains(img, "ghcr.io/schorsch3000/prebake:") {
			t.Errorf("%s: image should be the published one, overridable by PREBAKE_IMAGE: %q", f, img)
		}
	}
	b := dig(t, load(t, "../../docker-compose.build.yaml"), "services", "prebake")
	if dig(t, b, "build")["context"] != "." || b["image"] != "prebake:local" {
		t.Errorf("build override: %v", b)
	}
}

// The host's time zone is mounted read-only, without creating a missing host path, and the
// compose files must not set a TZ default (TZ="" would mean UTC to Go and hide the mount).
func TestHostTimeZoneMounted(t *testing.T) {
	for _, f := range []string{"../../docker-compose.yaml", "../../docker-compose.proxy.yaml"} {
		svc := dig(t, load(t, f), "services", "prebake")
		found := false
		for _, v := range svc["volumes"].([]any) {
			m, ok := v.(map[string]any)
			if !ok || m["target"] != "/etc/localtime" {
				continue
			}
			found = true
			if m["type"] != "bind" || m["read_only"] != true || dig(t, m, "bind")["create_host_path"] != false {
				t.Errorf("%s: /etc/localtime must be a read-only bind with create_host_path: false: %v", f, m)
			}
			if src, _ := m["source"].(string); !strings.Contains(src, "LOCALTIME_FILE") || !strings.Contains(src, "/etc/localtime") {
				t.Errorf("%s: unexpected source %q", f, src)
			}
		}
		if !found {
			t.Errorf("%s: no /etc/localtime mount", f)
		}
		tz, present := dig(t, svc, "environment")["TZ"]
		if !present || tz != nil {
			t.Errorf("%s: TZ must be a bare pass-through (no value), got %v", f, tz)
		}
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

// example.env must list every variable the compose files read, and nothing the
// compose files do not read (apart from COMPOSE_FILE, which Compose itself reads).
func TestExampleEnvMatchesComposeFiles(t *testing.T) {
	used := map[string]bool{}
	ref := regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)`)
	for _, f := range []string{"../../docker-compose.yaml", "../../docker-compose.proxy.yaml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range ref.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = true
		}
	}
	// bare pass-through entries (`TZ:` without value) read the variable from .env as well
	for _, f := range []string{"../../docker-compose.yaml", "../../docker-compose.proxy.yaml"} {
		for k, v := range dig(t, load(t, f), "services", "prebake", "environment") {
			if v == nil {
				used[k] = true
			}
		}
	}
	env, err := os.ReadFile("../../example.env")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	line := regexp.MustCompile(`(?m)^#? ?([A-Z][A-Z0-9_]*)=`)
	for _, m := range line.FindAllStringSubmatch(string(env), -1) {
		listed[m[1]] = true
	}
	for v := range used {
		if !listed[v] {
			t.Errorf("example.env does not mention %s, which the compose files use", v)
		}
	}
	for v := range listed {
		if !used[v] && v != "COMPOSE_FILE" {
			t.Errorf("example.env lists %s, which no compose file reads", v)
		}
	}
}
