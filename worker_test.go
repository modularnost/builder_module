package main

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

type buildkitWorkerSpec struct {
	SchemaVersion int      `json:"schema_version"`
	ServiceName   string   `json:"service_name"`
	Image         string   `json:"image"`
	Entrypoint    []string `json:"entrypoint"`
	Command       []string `json:"command"`
	Port          int      `json:"port"`
	Replicas      int      `json:"replicas"`
	UpdateOrder   string   `json:"update_order"`
	Placement     struct {
		PinToStorageNode bool `json:"pin_to_storage_node"`
	} `json:"placement"`
	Network struct {
		Name                string `json:"name"`
		Alias               string `json:"alias"`
		ExcludePanelNetwork bool   `json:"exclude_panel_network"`
	} `json:"network"`
	Isolation struct {
		Capabilities      []string `json:"capabilities"`
		Seccomp           string   `json:"seccomp"`
		AppArmor          string   `json:"apparmor"`
		BlockedSubnetsEnv string   `json:"blocked_subnets_env"`
	} `json:"isolation"`
	Storage struct {
		Volume string `json:"volume"`
		Target string `json:"target"`
	} `json:"storage"`
	Configs []struct {
		Source string `json:"source"`
		Target string `json:"target"`
		Mode   string `json:"mode"`
	} `json:"configs"`
	Secrets []struct {
		Name   string `json:"name"`
		Target string `json:"target"`
		Role   string `json:"role"`
		Mode   string `json:"mode"`
	} `json:"secrets"`
	Healthcheck struct {
		Test        []string `json:"test"`
		Interval    string   `json:"interval"`
		Timeout     string   `json:"timeout"`
		StartPeriod string   `json:"start_period"`
		Retries     int      `json:"retries"`
	} `json:"healthcheck"`
	Readiness struct {
		Probe            string `json:"probe"`
		Address          string `json:"address"`
		CASecret         string `json:"ca_secret"`
		ClientCertSecret string `json:"client_cert_secret"`
		ClientKeySecret  string `json:"client_key_secret"`
	} `json:"readiness"`
}

func TestBuildKitWorkerSpec(t *testing.T) {
	raw, err := os.ReadFile("deployment/buildkit-worker.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec buildkitWorkerSpec
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		t.Fatalf("decode buildkit-worker.json: %v", err)
	}

	if spec.SchemaVersion != 1 || spec.ServiceName != "buildkitd" || spec.Port != 1234 {
		t.Fatalf("unexpected worker header: %+v", spec)
	}
	if !strings.HasPrefix(spec.Image, "moby/buildkit:") || !strings.Contains(spec.Image, "@sha256:") {
		t.Fatalf("worker image must be digest-pinned moby/buildkit: %q", spec.Image)
	}
	dockerfile, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), spec.Image) || !strings.Contains(string(dockerfile), "deployment/buildkit-worker.json") {
		t.Fatal("Dockerfile must pin the same BuildKit image digest and package deployment/buildkit-worker.json")
	}

	if spec.Replicas != 1 || spec.UpdateOrder != "stop-first" || !spec.Placement.PinToStorageNode {
		t.Fatalf("expected single-replica stop-first pinned placement, got replicas=%d order=%q pin=%t", spec.Replicas, spec.UpdateOrder, spec.Placement.PinToStorageNode)
	}
	if spec.Network.Name != "buildnet" || spec.Network.Alias != "buildkitd" || !spec.Network.ExcludePanelNetwork {
		t.Fatalf("unexpected network spec: %+v", spec.Network)
	}
	if spec.Storage.Volume == "" || spec.Storage.Target != "/var/lib/buildkit" {
		t.Fatalf("unexpected storage spec: %+v", spec.Storage)
	}
	for _, capName := range []string{"SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE"} {
		if !slices.Contains(spec.Isolation.Capabilities, capName) {
			t.Fatalf("missing required isolation capability %q", capName)
		}
	}
	if spec.Isolation.Seccomp != "unconfined" || spec.Isolation.AppArmor != "disabled" || spec.Isolation.BlockedSubnetsEnv != "BUILDKIT_BLOCKED_SUBNETS" {
		t.Fatalf("unexpected isolation config: %+v", spec.Isolation)
	}
	if len(spec.Healthcheck.Test) < 2 || !strings.Contains(strings.Join(spec.Healthcheck.Test, " "), "debug workers") {
		t.Fatalf("healthcheck must verify BuildKit workers: %v", spec.Healthcheck.Test)
	}
	if spec.Readiness.Probe != "buildctl_debug_workers" || spec.Readiness.Address != "tcp://buildkitd:1234" ||
		spec.Readiness.CASecret == "" || spec.Readiness.ClientCertSecret == "" || spec.Readiness.ClientKeySecret == "" {
		t.Fatalf("unexpected readiness probe: %+v", spec.Readiness)
	}

	tomlRaw, err := os.ReadFile("scripts/buildkitd.toml")
	if err != nil {
		t.Fatal(err)
	}
	tomlText := string(tomlRaw)
	if !strings.Contains(tomlText, "unix:///run/buildkit/buildkitd.sock") || !strings.Contains(tomlText, "tcp://0.0.0.0:1234") {
		t.Fatal("buildkitd.toml must bind both tcp://0.0.0.0:1234 and unix:///run/buildkit/buildkitd.sock")
	}
	for _, secret := range spec.Secrets {
		if !strings.Contains(tomlText, secret.Target) {
			t.Fatalf("buildkitd.toml missing secret path %q", secret.Target)
		}
	}
	for _, cfg := range spec.Configs {
		if _, err := os.Stat(cfg.Source); err != nil {
			t.Fatalf("missing config source %q: %v", cfg.Source, err)
		}
	}

	netScript, err := os.ReadFile("scripts/buildkitd-network.sh")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(netScript, []byte("\r")) {
		t.Fatal("scripts/buildkitd-network.sh must use LF line endings")
	}
	if !strings.Contains(string(netScript), "/sys/fs/cgroup") || !strings.Contains(string(netScript), "BUILDKIT_BLOCKED_SUBNETS") {
		t.Fatal("scripts/buildkitd-network.sh must remount read-only cgroup and enforce BUILDKIT_BLOCKED_SUBNETS")
	}
}
