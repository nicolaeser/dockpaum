package updater

import (
	"fmt"
	"strings"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

var earliestPlausibleImage = time.Date(2013, time.March, 1, 0, 0, 0, 0, time.UTC)

const (
	ageAuto      = "auto"
	ageFirstSeen = "first_seen"
	ageCreated   = "created"
)

func policyAgeSources(cfg *config.Config) map[string]string {
	out := map[string]string{}
	for _, p := range cfg.Update.Policies {
		id := strings.ToLower(strings.TrimSpace(p.ID))
		if p.IsDefault() {
			id = "default"
		}
		out[id] = normAgeSource(p.MinImageAgeSource)
	}
	return out
}

func normAgeSource(s string) string {
	s = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "_")
	switch s {
	case ageFirstSeen, ageCreated:
		return s
	}
	return ageAuto
}

func (u *Updater) ageSourceOf(policyID string) string {
	if s, ok := u.ageSources[policyID]; ok {
		return s
	}
	if s, ok := u.ageSources["default"]; ok {
		return s
	}
	return ageAuto
}

type soak struct {
	hold       string
	eligibleAt time.Time
	firstSeen  time.Time
}

func (u *Updater) soakHold(host, ref, imageID string, created time.Time, err error, minAge time.Duration, source string) soak {
	now := u.now()
	var unusable string
	future := false
	switch {
	case err != nil:
		unusable = fmt.Sprintf("cannot read the creation time of the new image (%v)", err)
	case created.IsZero() || created.Before(earliestPlausibleImage):
		unusable = fmt.Sprintf("the new image reports the implausible creation time %s, as reproducible builds do",
			created.UTC().Format(time.RFC3339))
	case created.After(now):
		unusable = fmt.Sprintf("the new image reports a creation time in the future (%s)", created.UTC().Format(time.RFC3339))
		future = true
	}
	out := soak{firstSeen: u.state.FirstSeen(host, imageID, imageRepo(ref), now)}
	if source != ageFirstSeen && source != ageCreated {
		source = ageAuto
	}
	if source == ageFirstSeen && !u.state.Durable() {
		u.ageSourceWarning.Do(func() {
			u.deps.Logger.Warn("min_image_age is measured from the first sighting, but the state is not durable: " +
				"the creation time is used instead while it is plausible")
		})
		source = ageAuto
	}
	seenBy := "dockpaum"
	if !u.state.Durable() {
		seenBy = "this dockpaum process"
	}
	sinceSeen := fmt.Sprintf("since %s first saw it at %s", seenBy, out.firstSeen.UTC().Format(time.RFC3339))

	var start time.Time
	var basis, prefix string
	switch {
	case source == ageCreated && unusable != "":
		out.hold = fmt.Sprintf("%s; its age cannot be measured, so it is held (min %s)", unusable, minAge)
		return out
	case unusable != "" && !u.state.Durable() && !future:
		u.deps.Logger.Warn("min_image_age: the age of the new image cannot be measured without a durable state; it is not held",
			"host", host, "image", ref, "image_id", imageID, "reason", unusable)
		return out
	case unusable != "" && !u.state.Durable():
		start, prefix = created, unusable+"; "
	case unusable != "":
		start, basis, prefix = out.firstSeen, " "+sinceSeen, unusable+"; "
	case source == ageFirstSeen && out.firstSeen.After(created):
		start, basis = out.firstSeen, " "+sinceSeen
	default:
		start = created
	}
	out.eligibleAt = start.Add(minAge)
	if age := now.Sub(start); age < minAge {
		out.hold = fmt.Sprintf("%snew image age %s%s < min %s", prefix, age.Round(time.Second), basis, minAge)
	}
	return out
}
