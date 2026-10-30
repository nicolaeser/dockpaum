package updater

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/audit"
)

const (
	decisionGatedAutoRemove     audit.Decision = "gated-autoremove"
	decisionGatedOrchestrated   audit.Decision = "gated-orchestrated"
	decisionReattached          audit.Decision = "reattached"
	decisionSelfUpdateCompleted audit.Decision = "self-update-completed"
	decisionSelfUpdateFailed    audit.Decision = "self-update-failed"
	decisionCheckIncomplete     audit.Decision = "check-incomplete"
)

var orchestratorLabels = []struct{ key, name string }{
	{"com.docker.swarm.task.id", "Docker Swarm"},
	{"com.docker.swarm.service.id", "Docker Swarm"},
	{"io.kubernetes.", "Kubernetes"},
	{"com.hashicorp.nomad.", "Nomad"},
}

func orchestratedBy(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, o := range orchestratorLabels {
		for _, k := range keys {
			match := k == o.key
			if strings.HasSuffix(o.key, ".") {
				match = strings.HasPrefix(k, o.key)
			}
			if match {
				return fmt.Sprintf("managed by %s (label %s)", o.name, k)
			}
		}
	}
	return ""
}

const autoRemoveReason = "started with --rm (AutoRemove): stopping it would delete it, so it cannot be replaced safely"

func macAddressReason(mac, apiVersion string) string {
	return fmt.Sprintf("has the container-wide MAC address %s (--mac-address), which a replacement cannot keep on Docker API %s: "+
		"upgrade to Docker Engine 25 or newer (API 1.44), or remove --mac-address", mac, apiVersion)
}
