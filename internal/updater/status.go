package updater

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/registry"
	"github.com/nicolaeser/dockpaum/internal/state"
	"github.com/nicolaeser/dockpaum/internal/window"
)

const (
	UpdateAvailable = "available"
	UpdateNone      = "none"
	UpdateUnknown   = "unknown"
)

type ImageVersion struct {
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"`
	RepoDigest string `json:"repo_digest,omitempty"`
	Version    string `json:"version,omitempty"`
}

type Candidate struct {
	Image string `json:"image"`
	ImageVersion
	FoundAt    time.Time `json:"found_at"`
	FirstSeen  time.Time `json:"first_seen,omitzero"`
	Created    time.Time `json:"created,omitzero"`
	ReleaseURL string    `json:"release_url,omitempty"`
}

type ContainerStatus struct {
	Host         string          `json:"host"`
	Container    string          `json:"container"`
	ContainerID  string          `json:"container_id,omitempty"`
	Image        string          `json:"image"`
	Policy       string          `json:"policy,omitempty"`
	PolicySource string          `json:"policy_source,omitempty"`
	Mode         string          `json:"mode,omitempty"`
	Overrides    []string        `json:"overrides,omitempty"`
	MonitorOnly  bool            `json:"monitor_only,omitempty"`
	Current      ImageVersion    `json:"current"`
	Candidate    *Candidate      `json:"candidate,omitempty"`
	Update       string          `json:"update"`
	Pending      bool            `json:"pending"`
	Decision     string          `json:"decision"`
	Reason       string          `json:"reason,omitempty"`
	Error        string          `json:"error,omitempty"`
	CheckedAt    time.Time       `json:"checked_at"`
	EligibleAt   time.Time       `json:"eligible_at,omitzero"`
	NextWindow   time.Time       `json:"next_window,omitzero"`
	Check        string          `json:"check,omitempty"`
	CheckNote    string          `json:"check_note,omitempty"`
	Approval     *ApprovalStatus `json:"approval,omitempty"`
	RunID        string          `json:"run_id,omitempty"`
}

type ApprovalStatus struct {
	RequestID  string    `json:"request_id"`
	Phase      string    `json:"phase"`
	Approved   bool      `json:"approved"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	AskedAt    time.Time `json:"asked_at"`
	RetryAt    time.Time `json:"retry_at,omitzero"`
}

func (s ContainerStatus) clone() ContainerStatus {
	if s.Candidate != nil {
		c := *s.Candidate
		s.Candidate = &c
	}
	if s.Approval != nil {
		a := *s.Approval
		s.Approval = &a
	}
	s.Overrides = slices.Clone(s.Overrides)
	return s
}

type statusKey struct{ host, container string }

func (u *Updater) Containers() []ContainerStatus {
	u.statusMu.RLock()
	out := make([]ContainerStatus, 0, len(u.statuses))
	for _, s := range u.statuses {
		out = append(out, s.clone())
	}
	u.statusMu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host < out[j].Host
		}
		return out[i].Container < out[j].Container
	})
	return out
}

func (u *Updater) ContainerStatusOf(host, container string) (ContainerStatus, bool) {
	u.statusMu.RLock()
	defer u.statusMu.RUnlock()
	s, ok := u.statuses[statusKey{host, container}]
	if !ok {
		return ContainerStatus{}, false
	}
	return s.clone(), true
}

func (u *Updater) pendingUpdates() int {
	u.statusMu.RLock()
	defer u.statusMu.RUnlock()
	n := 0
	for _, s := range u.statuses {
		if s.Pending {
			n++
		}
	}
	return n
}

func (u *Updater) loadStatuses() {
	u.statusMu.Lock()
	defer u.statusMu.Unlock()
	for _, rec := range u.state.ContainerStatuses() {
		var s ContainerStatus
		if err := json.Unmarshal(rec.Status, &s); err != nil {
			u.deps.Logger.Debug("state: ignoring an unreadable container status", "host", rec.Host, "container", rec.Container, "err", err)
			continue
		}
		s.Host, s.Container = rec.Host, rec.Container
		u.statuses[statusKey{rec.Host, rec.Container}] = s
	}
}

func (u *Updater) putStatus(s ContainerStatus) {
	u.statusMu.Lock()
	u.statuses[statusKey{s.Host, s.Container}] = s
	u.statusMu.Unlock()
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	u.state.PutContainerStatus(s.Host, s.Container, raw)
}

func (u *Updater) retainStatuses(host string, keep map[string]bool) {
	var drop []string
	u.statusMu.Lock()
	for k := range u.statuses {
		if k.host == host && !keep[k.container] {
			delete(u.statuses, k)
			drop = append(drop, k.container)
		}
	}
	u.statusMu.Unlock()
	for _, name := range drop {
		u.state.DeleteContainerStatus(host, name)
	}
}

func (u *Updater) recordStatus(runID string, c *docker.Client, ct docker.Container, res stepResult, decision audit.Decision, err error) {
	if decision == "" {
		return
	}
	prev, hadPrev := u.ContainerStatusOf(c.Name, ct.Name)
	s := ContainerStatus{
		Host: c.Name, Container: ct.Name, ContainerID: ct.ID, Image: ct.Image, Policy: res.policyMode,
		PolicySource: res.policySource, Mode: res.mode, Overrides: slices.Clone(res.overrides), MonitorOnly: res.monitorOnly,
		Decision: string(decision), Reason: res.reason, CheckedAt: time.Now().UTC(), RunID: runID,
		Current: ImageVersion{Tag: registry.Parse(ct.Image).Tag, Digest: res.currentID,
			RepoDigest: res.currentRepoDigest, Version: res.currentVersion},
		Check: res.check, CheckNote: res.checkNote,
	}
	if s.Current.Digest == "" {
		s.Current.Digest = ct.ImageID
	}
	if res.image != "" {
		s.Image, s.Current.Tag = res.image, registry.Parse(res.image).Tag
	}
	if err != nil {
		s.Error = err.Error()
	}
	switch {
	case res.updated:
		s.Image = res.newImage
		s.Current = ImageVersion{Tag: registry.Parse(res.newImage).Tag, Digest: res.newDigest,
			RepoDigest: res.newRepoDigest, Version: res.newVersion}
		s.Update = UpdateNone
	case res.checked && res.candidate():
		s.Update, s.Pending = UpdateAvailable, true
		s.Candidate = &Candidate{
			Image: res.newImage,
			ImageVersion: ImageVersion{Tag: registry.Parse(res.newImage).Tag, Digest: res.newDigest,
				RepoDigest: res.newRepoDigest, Version: res.newVersion},
			FoundAt: s.CheckedAt, FirstSeen: res.firstSeen, Created: res.newCreated, ReleaseURL: res.releaseURL,
		}
		if hadPrev && prev.Candidate != nil && prev.Candidate.Digest == res.newDigest && prev.Candidate.Image == res.newImage {
			s.Candidate.FoundAt = prev.Candidate.FoundAt
		}
		s.EligibleAt = res.eligibleAt
	case res.checked:
		s.Update = UpdateNone
	case hadPrev && prev.Current.Digest == s.Current.Digest && prev.Update != "":
		s.Update, s.Pending, s.Candidate, s.EligibleAt = prev.Update, prev.Pending, prev.clone().Candidate, prev.EligibleAt
	default:
		s.Update = UpdateUnknown
	}
	s.NextWindow = u.nextWindow(res.windows, s.EligibleAt)
	s.Approval = res.approval
	if s.Approval == nil && !res.updated && hadPrev && prev.Approval != nil && prev.Approval.RetryAt.After(u.now()) {
		s.Approval = prev.Approval
	}
	u.putStatus(s)
}

type windowOpening struct{ from, next time.Time }

func (u *Updater) nextWindow(set *window.Set, eligibleAt time.Time) time.Time {
	from := u.windowTime()
	if eligibleAt.After(from) {
		from = eligibleAt
	}
	if set.Empty() || set.Allows(from) {
		return time.Time{}
	}
	u.nextOpenMu.Lock()
	defer u.nextOpenMu.Unlock()
	if u.nextOpen == nil {
		u.nextOpen = map[*window.Set]windowOpening{}
	}
	if o, ok := u.nextOpen[set]; ok && !from.Before(o.from) && from.Before(o.next) {
		return o.next
	}
	start := from.Truncate(time.Minute)
	for m := 1; m <= int(window.Week/time.Minute); m++ {
		t := start.Add(time.Duration(m) * time.Minute)
		if set.Allows(t) {
			u.nextOpen[set] = windowOpening{from: from, next: t}
			return t
		}
	}
	return time.Time{}
}

func (u *Updater) recordSelfUpdateStatus(ct docker.Container, found bool, rec audit.Record, err error) {
	if ct.Name == "" {
		return
	}
	prev, hadPrev := u.ContainerStatusOf(rec.Host, ct.Name)
	s := ContainerStatus{
		Host: rec.Host, Container: ct.Name, ContainerID: ct.ID, Image: ct.Image, Policy: rec.Policy,
		Decision: string(rec.Decision), Reason: rec.Reason, CheckedAt: time.Now().UTC(), RunID: rec.RunID,
		Current: ImageVersion{Tag: registry.Parse(ct.Image).Tag, Digest: ct.ImageID},
		Update:  UpdateUnknown,
	}
	if err != nil {
		s.Error = err.Error()
	}
	switch {
	case err == nil:
		s.Update = UpdateNone
	case hadPrev && found && prev.Current.Digest == ct.ImageID:
		s.Update, s.Pending, s.Candidate, s.EligibleAt = prev.Update, prev.Pending, prev.clone().Candidate, prev.EligibleAt
	}
	u.putStatus(s)
}

func (r stepResult) candidate() bool {
	return r.newDigest != "" && (r.newDigest != r.currentID || (r.newTag != "" && r.newTag != r.oldTag))
}

func liveOn(all []docker.Container) state.Live {
	var live state.Live
	for _, ct := range all {
		live.Containers = append(live.Containers, ct.Name)
		if m := tempSuffixRE.FindStringSubmatch(ct.Name); m != nil {
			live.Containers = append(live.Containers, m[1])
		}
		live.Images = append(live.Images, ct.ImageID)
		if repo := imageRepo(ct.Image); repo != "" && !strings.HasPrefix(ct.Image, "sha256:") {
			live.Repos = append(live.Repos, repo)
		} else {
			live.ReposUnknown = true
		}
	}
	return live
}

func imageRepo(ref string) string {
	r := registry.Parse(ref)
	if r.Repo == "" {
		return ""
	}
	return r.Registry + "/" + r.Repo
}

func DefaultInstance() string {
	name, err := os.Hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "dockpaum"
	}
	return name
}
