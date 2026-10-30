package updater

import (
	"sync"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/notify"
)

func decisionEvent(res stepResult) string {
	switch {
	case res.decision == audit.DecisionMonitorOnly, res.decision == audit.DecisionDryRun:
		if pendingUpdate(res) {
			return notify.EventAvailable
		}
	case res.decision == decisionCheckIncomplete:
		return notify.EventCheckFailed
	case res.decision == audit.DecisionSelfUpdate:
		return notify.EventSelfUpdateStarted
	case res.decision != audit.DecisionApprovalDeny && res.decision.Gated():
		if pendingUpdate(res) {
			return notify.EventNoUpdate
		}
	}
	return ""
}

func pendingUpdate(res stepResult) bool {
	return !res.checked || res.newDigest != res.oldDigest || (res.newTag != "" && res.newTag != res.oldTag)
}

func (u *Updater) warnIncomplete(host, container, image, reason string) {
	key := statusKey{host, container}
	u.incompleteMu.Lock()
	repeated := u.incomplete[key] == reason
	if reason == "" {
		delete(u.incomplete, key)
	} else {
		u.incomplete[key] = reason
	}
	u.incompleteMu.Unlock()
	if reason == "" {
		return
	}
	log := u.deps.Logger.Warn
	if repeated {
		log = u.deps.Logger.Debug
	}
	log("tag check incomplete: no other tag in the registry has this tag's form, so whether a newer version exists is unknown "+
		"(the variant may have been renamed or dropped upstream)",
		"host", host, "container", container, "image", image, "reason", reason)
}

func (u *Updater) retainLabelNotes(host string, all []docker.Container) {
	live := make(map[string]bool, len(all))
	for _, ct := range all {
		live[ct.Name] = true
	}
	prune := func(mu *sync.Mutex, notes map[statusKey]string) {
		mu.Lock()
		defer mu.Unlock()
		for k := range notes {
			if k.host == host && !live[k.container] {
				delete(notes, k)
			}
		}
	}
	prune(&u.incompleteMu, u.incomplete)
	prune(&u.fallbackMu, u.fallback)
}
