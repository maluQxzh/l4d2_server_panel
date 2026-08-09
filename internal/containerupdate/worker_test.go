package containerupdate

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestReplacementRequestUsesAllowlistedComposeConfiguration(t *testing.T) {
	current := containerInspect{
		Config: map[string]any{
			"Labels": map[string]any{
				"com.docker.compose.project": "l4d2panel",
				"com.docker.compose.service": "app",
				"com.docker.compose.image":   "sha256:old",
				"untrusted.runtime.label":    "discard-me",
			},
			"Env": []any{
				"DATABASE_PATH=/app/data/l4d2panel.sqlite3", "MASTER_KEY=", "MASTER_KEY_FILE=/app/data/control-master.key",
				"CONTAINER_UPDATE_ENABLED=true", "CONTAINER_DOCKER_SOCKET=/var/run/docker.sock", "CONTAINER_UPDATE_ROOT=/app/updates",
				"PUBLIC_URL=https://localhost", "COOKIE_SECURE=true", "PATH=/old-image/bin",
			},
			"Entrypoint": []any{"/old-entrypoint"},
			"Cmd":        []any{"old-command"},
		},
		HostConfig: map[string]any{
			"PortBindings": map[string]any{"19090/tcp": []any{map[string]any{"HostIp": "127.0.0.1", "HostPort": "19090"}}},
			"Privileged":   true,
			"CgroupParent": "daemon-generated",
		},
		Mounts: []dockerMount{
			{Type: "volume", Name: "l4d2panel_app_data", Destination: "/app/data"},
			{Type: "volume", Name: "l4d2panel_app_spool", Destination: "/app/spool"},
			{Type: "volume", Name: "l4d2panel_app_updates", Destination: "/app/updates"},
			{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
		},
	}
	current.NetworkSettings.Networks = map[string]map[string]any{
		"l4d2panel_default": {"Aliases": []any{"old-runtime-alias"}, "IPAMConfig": map[string]any{"IPv4Address": "172.20.0.4"}, "GwPriority": 12},
	}

	request, err := replacementRequest(current, "sha256:new")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"Entrypoint", "Cmd", "Hostname", "Domainname"} {
		if _, exists := request[forbidden]; exists {
			t.Fatalf("replacement request replayed %s from the old image", forbidden)
		}
	}
	host := request["HostConfig"].(map[string]any)
	for _, forbidden := range []string{"Privileged", "CgroupParent"} {
		if _, exists := host[forbidden]; exists {
			t.Fatalf("replacement HostConfig replayed %s from inspect", forbidden)
		}
	}
	if host["NetworkMode"] != "l4d2panel_default" || host["ReadonlyRootfs"] != true {
		t.Fatalf("unexpected constrained HostConfig: %#v", host)
	}
	labels := request["Labels"].(map[string]string)
	if labels["untrusted.runtime.label"] != "" || labels["com.docker.compose.image"] != "" || labels["com.docker.compose.service"] != "app" {
		t.Fatalf("unexpected replacement labels: %#v", labels)
	}
	environment := request["Env"].([]string)
	if strings.Contains(strings.Join(environment, "\n"), "PATH=/old-image/bin") {
		t.Fatalf("old image defaults leaked into replacement environment: %#v", environment)
	}
	endpoint := request["NetworkingConfig"].(map[string]any)["EndpointsConfig"].(map[string]any)["l4d2panel_default"].(map[string]any)
	if strings.Join(endpoint["Aliases"].([]string), ",") != "app" || endpoint["IPAMConfig"] != nil || endpoint["GwPriority"] != nil {
		t.Fatalf("daemon-generated endpoint configuration was replayed: %#v", endpoint)
	}
}

func TestReplacementRequestRejectsUnsafeRuntimeShape(t *testing.T) {
	current := validReplacementInspect()
	current.HostConfig["PortBindings"] = map[string]any{"19090/tcp": []any{map[string]any{"HostIp": "localhost", "HostPort": "19090"}}}
	if _, err := replacementRequest(current, "sha256:new"); err == nil || !strings.Contains(err.Error(), "host port") {
		t.Fatalf("invalid host binding error = %v", err)
	}
	current = validReplacementInspect()
	current.Mounts[0].Name = "../escaped"
	if _, err := replacementRequest(current, "sha256:new"); err == nil || !strings.Contains(err.Error(), "mounts") {
		t.Fatalf("invalid volume name error = %v", err)
	}
}

func TestDockerLogTextAndDiagnosticSanitization(t *testing.T) {
	frame := func(stream byte, text string) []byte {
		result := make([]byte, 8+len(text))
		result[0] = stream
		binary.BigEndian.PutUint32(result[4:8], uint32(len(text)))
		copy(result[8:], text)
		return result
	}
	data := append(frame(1, "starting\n"), frame(2, "password=hunter2 bearer abc.def\n")...)
	decoded := dockerLogText(data)
	if decoded != "starting\npassword=hunter2 bearer abc.def\n" {
		t.Fatalf("decoded logs = %q", decoded)
	}
	diagnostic := sanitizeError(errors.New(decoded))
	if strings.Contains(diagnostic, "hunter2") || strings.Contains(diagnostic, "abc.def") || !strings.Contains(diagnostic, "[redacted]") {
		t.Fatalf("diagnostic was not redacted: %q", diagnostic)
	}
}

func validReplacementInspect() containerInspect {
	current := containerInspect{
		Config: map[string]any{
			"Labels": map[string]any{"com.docker.compose.project": "l4d2panel", "com.docker.compose.service": "app"},
			"Env":    []any{"DATABASE_PATH=/app/data/l4d2panel.sqlite3", "MASTER_KEY_FILE=/app/data/control-master.key", "CONTAINER_UPDATE_ENABLED=true", "CONTAINER_DOCKER_SOCKET=/var/run/docker.sock", "CONTAINER_UPDATE_ROOT=/app/updates"},
		},
		HostConfig: map[string]any{"PortBindings": map[string]any{"19090/tcp": []any{map[string]any{"HostIp": "127.0.0.1", "HostPort": "19090"}}}},
		Mounts: []dockerMount{
			{Type: "volume", Name: "l4d2panel_app_data", Destination: "/app/data"},
			{Type: "volume", Name: "l4d2panel_app_spool", Destination: "/app/spool"},
			{Type: "volume", Name: "l4d2panel_app_updates", Destination: "/app/updates"},
			{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
		},
	}
	current.NetworkSettings.Networks = map[string]map[string]any{"l4d2panel_default": {}}
	return current
}
