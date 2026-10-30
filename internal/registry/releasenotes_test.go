package registry

import "testing"

func TestReleaseNotesURL(t *testing.T) {
	const (
		src  = "org.opencontainers.image.source"
		home = "org.opencontainers.image.url"
		ver  = "org.opencontainers.image.version"
	)
	tests := []struct {
		name     string
		labels   map[string]string
		tag      string
		template string
		want     string
	}{
		{
			name:   "github semver gets a v tag",
			labels: map[string]string{src: "https://github.com/acme/app"},
			tag:    "1.27.3",
			want:   "https://github.com/acme/app/releases/tag/v1.27.3",
		},
		{
			name:   "v tag kept",
			labels: map[string]string{src: "https://github.com/acme/app"},
			tag:    "v2.8.4",
			want:   "https://github.com/acme/app/releases/tag/v2.8.4",
		},
		{
			name:   "prerelease kept",
			labels: map[string]string{src: "https://github.com/acme/app"},
			tag:    "1.3.0-rc.1",
			want:   "https://github.com/acme/app/releases/tag/v1.3.0-rc.1",
		},
		{
			name:   "variant dropped",
			labels: map[string]string{src: "https://github.com/acme/app"},
			tag:    "1.27.3-alpine",
			want:   "https://github.com/acme/app/releases/tag/v1.27.3",
		},
		{
			name:   "calver without v and with leading zeros",
			labels: map[string]string{src: "https://github.com/pi-hole/docker-pi-hole"},
			tag:    "2024.07.0",
			want:   "https://github.com/pi-hole/docker-pi-hole/releases/tag/2024.07.0",
		},
		{
			name:   "linuxserver tag kept whole",
			labels: map[string]string{src: "https://github.com/linuxserver/docker-radarr", ver: "5.14.0.9383-ls245"},
			tag:    "5.14.0.9383-ls245",
			want:   "https://github.com/linuxserver/docker-radarr/releases/tag/5.14.0.9383-ls245",
		},
		{
			name:   "tag-follow uses the version label",
			labels: map[string]string{src: "https://github.com/acme/app", ver: "1.27.3"},
			tag:    "latest",
			want:   "https://github.com/acme/app/releases/tag/v1.27.3",
		},
		{
			name:   "version label refines a floating tag",
			labels: map[string]string{src: "https://github.com/acme/app", ver: "1.27.3"},
			tag:    "1.27",
			want:   "https://github.com/acme/app/releases/tag/v1.27.3",
		},
		{
			name:   "contradicting version label from a base image is ignored",
			labels: map[string]string{src: "https://github.com/acme/app", ver: "3.20.0"},
			tag:    "1.27.3",
			want:   "https://github.com/acme/app/releases/tag/v1.27.3",
		},
		{
			name:   "no version links the release list",
			labels: map[string]string{src: "https://github.com/acme/app"},
			tag:    "latest",
			want:   "https://github.com/acme/app/releases",
		},
		{
			name:   "git suffix trailing slash and www",
			labels: map[string]string{src: "https://www.github.com/acme/app.git/"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "http github upgraded to https",
			labels: map[string]string{src: "http://github.com/acme/app"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "userinfo in the label is dropped",
			labels: map[string]string{src: "https://bot@github.com/acme/app"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "scp-like git url",
			labels: map[string]string{src: "git@github.com:acme/app.git"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "scheme-less url",
			labels: map[string]string{src: "github.com/acme/app"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "git+https url",
			labels: map[string]string{src: "git+https://github.com/acme/app.git"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "ssh url drops user and ssh port",
			labels: map[string]string{src: "ssh://git@github.com:22/acme/app.git"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "web port dropped",
			labels: map[string]string{src: "https://github.com:443/acme/app"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "gitlab nested groups",
			labels: map[string]string{src: "https://gitlab.com/acme/platform/app"},
			tag:    "1.4.0",
			want:   "https://gitlab.com/acme/platform/app/-/releases/v1.4.0",
		},
		{
			name:   "gitlab release list and http upgraded to https",
			labels: map[string]string{src: "http://gitlab.com/team/app.git"},
			tag:    "latest",
			want:   "https://gitlab.com/team/app/-/releases",
		},
		{
			name:   "gitlab project named like a page as the second segment",
			labels: map[string]string{src: "https://gitlab.com/acme/issues"},
			tag:    "1.0.0",
			want:   "https://gitlab.com/acme/issues/-/releases/v1.0.0",
		},
		{
			name:   "self-hosted gitlab needs a template",
			labels: map[string]string{src: "http://gitlab.internal.example:8080/team/app"},
			tag:    "latest",
			want:   "",
		},
		{
			name:   "codeberg needs a template",
			labels: map[string]string{src: "https://codeberg.org/forgejo/forgejo"},
			tag:    "v9.0.0",
			want:   "",
		},
		{
			name:   "gitea host needs a template",
			labels: map[string]string{src: "ssh://git@gitea.example.com:2222/acme/app.git"},
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:   "forgejo host needs a template",
			labels: map[string]string{src: "https://forgejo.example.org/acme/app"},
			tag:    "2.0.0",
			want:   "",
		},
		{
			name:   "url label as fallback",
			labels: map[string]string{src: "https://example.com/acme/app", home: "https://github.com/acme/app"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "monorepo subpath is not a release page",
			labels: map[string]string{src: "https://github.com/bitnami/containers/tree/main/bitnami/postgresql"},
			tag:    "16.4.1-debian-12-r0",
			want:   "",
		},
		{
			name:   "gitlab non-project path",
			labels: map[string]string{src: "https://gitlab.com/acme/app/-/tree/main"},
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:   "gitlab legacy tree path without separator",
			labels: map[string]string{src: "https://gitlab.com/gitlab-org/gitlab/tree/master"},
			tag:    "17.0.0",
			want:   "",
		},
		{
			name:   "gitlab legacy blob path in a nested group",
			labels: map[string]string{src: "https://gitlab.com/acme/platform/app/blob/main/Dockerfile"},
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:   "gitlab legacy page name is case-insensitive",
			labels: map[string]string{src: "https://gitlab.com/acme/app/Merge_Requests"},
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:   "github owner only",
			labels: map[string]string{src: "https://github.com/acme"},
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:   "unknown host",
			labels: map[string]string{src: "https://git.example.com/acme/app", home: "https://hub.docker.com/_/nginx"},
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:   "host merely containing github",
			labels: map[string]string{src: "https://github.com.evil.example/acme/app"},
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:   "unsupported scheme",
			labels: map[string]string{src: "javascript://github.com/acme/app"},
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:   "dot segments",
			labels: map[string]string{src: "https://github.com/acme/.."},
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:   "no labels",
			labels: nil,
			tag:    "1.0.0",
			want:   "",
		},
		{
			name:     "template wins",
			labels:   map[string]string{src: "https://github.com/acme/app"},
			tag:      "1.27.3",
			template: "https://acme.example/changelog#${major}-${minor}-${patch}",
			want:     "https://acme.example/changelog#1-27-3",
		},
		{
			name:     "template with tag and version",
			labels:   map[string]string{ver: "1.27.3"},
			tag:      "1.27",
			template: "https://acme.example/${tag}/notes?v=${version}",
			want:     "https://acme.example/1.27/notes?v=1.27.3",
		},
		{
			name:     "template keeps leading zeros",
			tag:      "2024.07.0",
			template: "https://acme.example/${major}.${minor}",
			want:     "https://acme.example/2024.07",
		},
		{
			name:     "template placeholder without value",
			tag:      "1.27",
			template: "https://acme.example/${patch}",
			want:     "",
		},
		{
			name:     "template unknown placeholder",
			tag:      "1.27.3",
			template: "https://acme.example/${build}",
			want:     "",
		},
		{
			name:     "template must be http",
			tag:      "1.27.3",
			template: "javascript:alert(${major})",
			want:     "",
		},
		{
			name:     "template must not carry userinfo",
			tag:      "1.27.3",
			template: "https://user@acme.example/${tag}",
			want:     "",
		},
		{
			name:     "template values are path-escaped",
			tag:      "a/../b",
			template: "https://acme.example/${tag}",
			want:     "https://acme.example/a%2F..%2Fb",
		},
		{
			name:     "template covers a self-hosted forge",
			labels:   map[string]string{src: "https://codeberg.org/forgejo/forgejo"},
			tag:      "v9.0.0",
			template: "https://codeberg.org/forgejo/forgejo/releases/tag/${tag}",
			want:     "https://codeberg.org/forgejo/forgejo/releases/tag/v9.0.0",
		},
		{
			name:     "blank template derives the link",
			labels:   map[string]string{src: "https://github.com/acme/app"},
			tag:      "1.0.0",
			template: "  ",
			want:     "https://github.com/acme/app/releases/tag/v1.0.0",
		},
		{
			name:   "a template label among the image labels is not read",
			labels: map[string]string{src: "https://github.com/acme/app", "dockpaum.link.template": "https://acme.example/${tag}"},
			tag:    "1.0.0",
			want:   "https://github.com/acme/app/releases/tag/v1.0.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReleaseNotesURL(tt.labels, tt.tag, tt.template); got != tt.want {
				t.Errorf("ReleaseNotesURL(%v, %q, %q) = %q, want %q", tt.labels, tt.tag, tt.template, got, tt.want)
			}
		})
	}
}
