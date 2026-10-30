package registry

import "testing"

func TestSelectsImage(t *testing.T) {
	cases := []struct {
		want  []string
		image string
		match bool
	}{
		{nil, "anything:1", true},
		{[]string{"nginx"}, "nginx", true},
		{[]string{"nginx"}, "nginx:1.27", true},
		{[]string{"nginx"}, "docker.io/library/nginx:1.27", true},
		{[]string{"docker.io/nginx"}, "nginx:latest", true},
		{[]string{"library/nginx"}, "index.docker.io/library/nginx", true},
		{[]string{"nginx"}, "nginx@sha256:abc", true},
		{[]string{" nginx "}, "nginx:1", true},
		{[]string{"nginx"}, "nginxinc/nginx-unprivileged", false},
		{[]string{"nginx"}, "ghcr.io/library/nginx", false},
		{[]string{"nginx:1.27"}, "nginx:1.27", true},
		{[]string{"nginx:1.27"}, "nginx:1.26", false},
		{[]string{"nginx:latest"}, "nginx", true},
		{[]string{"nginx:latest"}, "nginx@sha256:abc", false},
		{[]string{"nginx@sha256:abc"}, "nginx@sha256:abc", true},
		{[]string{"nginx@sha256:abc"}, "nginx:latest", false},
		{[]string{"ghcr.io/acme/api"}, "ghcr.io/acme/api:2.0", true},
		{[]string{"GHCR.io/acme/api"}, "ghcr.io/acme/api", true},
		{[]string{"localhost:5000/app"}, "localhost:5000/app:dev", true},
		{[]string{"localhost:5000/app"}, "localhost:5001/app:dev", false},
		{[]string{"redis", "postgres"}, "postgres:16", true},
		{[]string{"redis", "postgres"}, "mysql:8", false},
	}
	for _, c := range cases {
		if got := SelectsAnyImage(c.want, c.image); got != c.match {
			t.Errorf("SelectsAnyImage(%q, %q) = %v, want %v", c.want, c.image, got, c.match)
		}
	}
}
