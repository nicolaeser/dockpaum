package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
)

const (
	testDigestOld = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testDigestNew = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestPullByDigestLeavesTheTag(t *testing.T) {
	for _, store := range []string{"classic", "containerd"} {
		t.Run(store, func(t *testing.T) {
			d := dockertest.New(t, "1.47")
			if store == "containerd" {
				d.UseContainerdStore()
			}
			d.AddImage(dockertest.Image{ID: "old", RepoDigests: []string{"example.com/app@" + testDigestOld}}, "example.com/app:1")
			d.Publish("example.com/app:1", dockertest.Image{ID: "new", RepoDigests: []string{"example.com/app@" + testDigestNew}})
			c := testClient(t, d)
			ctx := context.Background()

			id, err := c.PullImagePlatform(ctx, "example.com/app@"+testDigestNew, nil, "")
			if err != nil || id != "sha256:new" {
				t.Fatalf("pull by digest = %q, %v", id, err)
			}
			if got := d.ImageID("example.com/app:1"); got != "sha256:old" {
				t.Fatalf("a pull by digest moved the tag to %s", got)
			}
			if err := c.RemoveImageIfDangling(ctx, "sha256:new"); err != nil || d.HasImage("new") {
				t.Fatalf("untagged digest-pulled image kept: %v", err)
			}
			if _, err := c.PullImagePlatform(ctx, "example.com/app@"+testDigestOld, nil, ""); err == nil {
				t.Fatal("a digest no published image has must not pull")
			}
		})
	}
}

func TestRemoveImageIfDanglingKeepsUsedAndTaggedImages(t *testing.T) {
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{ID: "used", RepoDigests: []string{"example.com/app@" + testDigestOld}}, "example.com/app:1")
	d.AddImage(dockertest.Image{ID: "tagged"}, "example.com/app:2")
	d.Run("web", &container.Config{Image: "example.com/app:1"}, nil, nil)
	c := testClient(t, d)
	ctx := context.Background()
	if err := c.ImageTag(ctx, "sha256:tagged", "example.com/app:1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveImageIfDangling(ctx, "sha256:used"); err == nil || !d.HasImage("used") {
		t.Fatalf("an image in use must be kept: %v", err)
	}
	if err := c.RemoveImageIfDangling(ctx, "sha256:tagged"); err != nil || !d.HasImage("tagged") {
		t.Fatalf("a tagged image must be kept: %v", err)
	}
	if err := c.RemoveImageIfDangling(ctx, "sha256:gone"); err == nil || !strings.Contains(err.Error(), "No such image") {
		t.Fatalf("an image that is gone already: %v", err)
	}
}

func TestImagePlatformAndManifest(t *testing.T) {
	d := dockertest.New(t, "1.48")
	d.AddImage(dockertest.Image{ID: "arm", Architecture: "arm", Variant: "v6", Manifest: testDigestNew}, "example.com/app:1")
	d.AddImage(dockertest.Image{ID: "plain"}, "example.com/plain:1")
	d.Run("arm", &container.Config{Image: "example.com/app:1"}, nil, nil)
	d.Run("plain", &container.Config{Image: "example.com/plain:1"}, nil, nil)
	c := testClient(t, d)
	ctx := context.Background()

	arm, err := c.Inspect(ctx, "arm")
	if err != nil {
		t.Fatal(err)
	}
	if got := arm.ImageManifestDigest(); got != testDigestNew {
		t.Fatalf("ImageManifestDigest = %q", got)
	}
	if p, err := c.ImagePlatform(ctx, arm); err != nil || p != "linux/arm/v6" {
		t.Fatalf("ImagePlatform = %q, %v", p, err)
	}
	plain, err := c.Inspect(ctx, "plain")
	if err != nil {
		t.Fatal(err)
	}
	if plain.ImageManifestDigest() != "" {
		t.Fatal("a daemon that does not report the manifest gives no digest")
	}
	if p, err := c.ImagePlatform(ctx, plain); err != nil || p != "linux/amd64" {
		t.Fatalf("ImagePlatform = %q, %v", p, err)
	}
	d.DropImage("plain")
	if _, err := c.ImagePlatform(ctx, plain); err == nil || !strings.Contains(err.Error(), "inspect image") {
		t.Fatalf("a vanished image: %v", err)
	}
}
