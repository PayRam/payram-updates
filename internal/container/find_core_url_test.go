package container

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeDockerInspect writes a docker stand-in whose `inspect` prints a
// container publishing the given container->host ports.
func fakeDockerInspect(t *testing.T, ports map[string]string) string {
	t.Helper()
	var bindings []string
	for containerPort, hostPort := range ports {
		bindings = append(bindings, fmt.Sprintf(`"%s/tcp":[{"HostIp":"0.0.0.0","HostPort":"%s"}]`, containerPort, hostPort))
	}
	inspect := fmt.Sprintf(`[{"Id":"abc","Name":"/payram","Image":"sha256:1","Config":{"Image":"payramapp/payram:3.10.0","Env":[]},"HostConfig":{"NetworkMode":"bridge","PortBindings":{%s}},"Mounts":[],"NetworkSettings":{"Networks":{}}}]`, strings.Join(bindings, ","))
	script := filepath.Join(t.TempDir(), "docker")
	body := "#!/bin/sh\nif [ \"$1\" = inspect ]; then cat <<'JSON'\n" + inspect + "\nJSON\nexit 0; fi\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return script
}

func portOf(t *testing.T, server *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func healthHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == HealthPath {
			w.Write([]byte(`{"status":"ok","db":"ok"}`))
			return
		}
		w.Write([]byte(`<html>PayRam dashboard</html>`))
	})
}

func TestFindCoreBaseURL(t *testing.T) {
	quiet := log.New(io.Discard, "", 0)
	ctx := context.Background()

	t.Run("configured address wins", func(t *testing.T) {
		url, how, err := FindCoreBaseURL(ctx, "false", "payram", "https://payram.example:8443", quiet)
		if err != nil || url != "https://payram.example:8443" || how != CoreURLConfigured {
			t.Fatalf("got %q %q %v", url, how, err)
		}
	})

	t.Run("bundled nginx on 443 answers over https", func(t *testing.T) {
		https := httptest.NewTLSServer(healthHandler())
		defer https.Close()
		docker := fakeDockerInspect(t, map[string]string{"443": portOf(t, https)})

		url, how, err := FindCoreBaseURL(ctx, docker, "payram", "", quiet)
		if err != nil || how != CoreURLAnswered || url != "https://127.0.0.1:"+portOf(t, https) {
			t.Fatalf("got %q %q %v", url, how, err)
		}
	})

	t.Run("http-only install on 80", func(t *testing.T) {
		plain := httptest.NewServer(healthHandler())
		defer plain.Close()
		docker := fakeDockerInspect(t, map[string]string{"80": portOf(t, plain)})

		url, how, err := FindCoreBaseURL(ctx, docker, "payram", "", quiet)
		if err != nil || how != CoreURLAnswered || url != "http://127.0.0.1:"+portOf(t, plain) {
			t.Fatalf("got %q %q %v", url, how, err)
		}
	})

	t.Run("PayRam down: the https port is guessed, never a port that is not published", func(t *testing.T) {
		docker := fakeDockerInspect(t, map[string]string{"80": "1", "443": "2"})

		url, how, err := FindCoreBaseURL(ctx, docker, "payram", "", quiet)
		if err != nil || how != CoreURLGuessed || url != "https://127.0.0.1:2" {
			t.Fatalf("got %q %q %v, want the published 443 guessed", url, how, err)
		}
	})

	t.Run("no PayRam port published", func(t *testing.T) {
		docker := fakeDockerInspect(t, map[string]string{"5432": "5432"})
		if _, _, err := FindCoreBaseURL(ctx, docker, "payram", "", quiet); err == nil || !strings.Contains(err.Error(), "CORE_BASE_URL") {
			t.Fatalf("got %v, want an error that names CORE_BASE_URL", err)
		}
	})
}
