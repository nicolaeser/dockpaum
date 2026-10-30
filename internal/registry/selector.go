package registry

import "strings"

func SelectsImage(want, image string) bool {
	w, h := Parse(strings.TrimSpace(want)), Parse(strings.TrimSpace(image))
	if canonicalHost(w.Registry) != canonicalHost(h.Registry) || w.Repo != h.Repo {
		return false
	}
	switch {
	case w.Digest != "":
		return w.Digest == h.Digest
	case w.Tag != "":
		tag := h.Tag
		if tag == "" && h.Digest == "" {
			tag = "latest"
		}
		return w.Tag == tag
	}
	return true
}

func SelectsAnyImage(filters []string, image string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if SelectsImage(f, image) {
			return true
		}
	}
	return false
}
