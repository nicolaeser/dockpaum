package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func (c *Client) ForeignPlatform(ctx context.Context, fc *FullContainer) (string, error) {
	native, err := c.nativePlatform(ctx)
	if err != nil {
		return "", fmt.Errorf("daemon platform: %w", err)
	}
	img := fc.imagePlatform
	if img == nil {
		ictx, cancel := opCtx(ctx, opTimeout)
		defer cancel()
		insp, err := c.api.ImageInspect(ictx, fc.ImageID)
		if err != nil {
			return "", fmt.Errorf("inspect image %s: %w", fc.ImageID, err)
		}
		img = &ocispec.Platform{OS: insp.Os, Architecture: insp.Architecture, Variant: insp.Variant}
	}
	return foreignPlatform(native, img), nil
}

func (c *Client) ImagePlatform(ctx context.Context, fc *FullContainer) (string, error) {
	img := fc.imagePlatform
	if img == nil {
		ictx, cancel := opCtx(ctx, opTimeout)
		defer cancel()
		insp, err := c.api.ImageInspect(ictx, fc.ImageID)
		if err != nil {
			return "", fmt.Errorf("inspect image %s: %w", fc.ImageID, err)
		}
		img = &ocispec.Platform{OS: insp.Os, Architecture: insp.Architecture, Variant: insp.Variant}
	}
	if img.OS == "" || img.Architecture == "" {
		return "", fmt.Errorf("image %s does not record its platform", fc.ImageID)
	}
	return formatPlatform(img), nil
}

func foreignPlatform(native string, img *ocispec.Platform) string {
	if img == nil || img.OS == "" || img.Architecture == "" {
		return ""
	}
	if native == img.OS+"/"+img.Architecture {
		return ""
	}
	return formatPlatform(img)
}

func (c *Client) nativePlatform(ctx context.Context) (string, error) {
	c.nativeMu.Lock()
	defer c.nativeMu.Unlock()
	if c.native != "" {
		return c.native, nil
	}
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	v, err := c.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return "", err
	}
	if v.Os == "" || v.Arch == "" {
		return "", fmt.Errorf("the daemon did not report its platform")
	}
	c.native = v.Os + "/" + v.Arch
	return c.native, nil
}

func formatPlatform(p *ocispec.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func parsePlatform(s string) *ocispec.Platform {
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	p := &ocispec.Platform{OS: parts[0], Architecture: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	return p
}
