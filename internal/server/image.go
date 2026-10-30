package server

import (
	"strings"

	"github.com/nicolaeser/dockpaum/internal/registry"
)

func (r TriggerRequest) MatchesImage(image string) bool {
	if len(r.Images) == 0 {
		return true
	}
	have := parseImage(image)
	for _, want := range r.Images {
		if parseImage(want).selects(have) {
			return true
		}
	}
	return false
}

type imageRef struct {
	repo   string
	tag    string
	digest string
}

func parseImage(ref string) imageRef {
	p := registry.Parse(strings.TrimSpace(ref))
	host, repo := strings.ToLower(p.Registry), p.Repo
	switch host {
	case "docker.io", "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		host = "docker.io"
		if !strings.Contains(repo, "/") {
			repo = "library/" + repo
		}
	}
	return imageRef{repo: host + "/" + repo, tag: p.Tag, digest: p.Digest}
}

func (want imageRef) selects(have imageRef) bool {
	if want.repo != have.repo {
		return false
	}
	switch {
	case want.digest != "":
		return want.digest == have.digest
	case want.tag != "":
		tag := have.tag
		if tag == "" && have.digest == "" {
			tag = "latest"
		}
		return want.tag == tag
	}
	return true
}
