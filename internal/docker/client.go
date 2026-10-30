package docker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/versions"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/nicolaeser/dockpaum/internal/config"
)

const (
	opTimeout   = 30 * time.Second
	bulkTimeout = 2 * time.Minute
	pullTimeout = 15 * time.Minute
)

func opCtx(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}

type Client struct {
	Name string
	api  *client.Client

	nativeMu sync.Mutex
	native   string
}

type Clients struct {
	mu          sync.Mutex
	reconnectMu sync.Mutex
	list        []*Client
	failed      []config.HostConfig
	logger      *slog.Logger
}

func OpenAll(ctx context.Context, hosts []config.HostConfig, logger *slog.Logger) (*Clients, error) {
	out := &Clients{logger: logger}
	var firstErr error
	for _, h := range hosts {
		if h.TLS != nil && !h.TLS.ShouldVerify() {
			logger.Warn("TLS certificate verification is DISABLED for this host", "host", h.Name, "endpoint", h.Endpoint)
		}
		c, err := open(ctx, h)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("host %s: %w", h.Name, err)
			}
			logger.Error("docker connect failed, host will be retried each run", "host", h.Name, "err", err)
			out.failed = append(out.failed, h)
			continue
		}
		logger.Info("docker connected", "host", h.Name, "endpoint", h.Endpoint)
		out.list = append(out.list, c)
	}
	if len(out.list) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (cs *Clients) Reconnect(ctx context.Context) {
	cs.reconnectMu.Lock()
	defer cs.reconnectMu.Unlock()
	cs.mu.Lock()
	pending := append([]config.HostConfig(nil), cs.failed...)
	cs.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	var still []config.HostConfig
	var opened []*Client
	for _, h := range pending {
		c, err := open(ctx, h)
		if err != nil {
			cs.logger.Warn("docker reconnect failed", "host", h.Name, "err", err)
			still = append(still, h)
			continue
		}
		cs.logger.Info("docker reconnected", "host", h.Name, "endpoint", h.Endpoint)
		opened = append(opened, c)
	}
	cs.mu.Lock()
	cs.failed = still
	cs.list = append(cs.list, opened...)
	cs.mu.Unlock()
}

func open(ctx context.Context, h config.HostConfig) (*Client, error) {
	if strings.HasPrefix(h.Endpoint, "ssh://") {
		return nil, fmt.Errorf("ssh:// endpoints are not supported; use a local socket or tcp:// (optionally with TLS)")
	}
	var opts []client.Opt
	if h.Endpoint != "" {
		opts = append(opts, client.WithHost(h.Endpoint))
	} else {
		opts = append(opts, client.FromEnv)
	}
	if h.TLS != nil {
		hc, err := tlsHTTPClient(h.TLS)
		if err != nil {
			return nil, err
		}
		opts = append(opts, client.WithHTTPClient(hc))
	}
	api, err := client.New(opts...)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	if _, err := api.Ping(pingCtx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		_ = api.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Client{Name: h.Name, api: api}, nil
}

func (c *Client) APIVersion() string {
	return c.api.ClientVersion()
}

func (c *Client) apiAtLeast(v string) bool {
	cur := c.api.ClientVersion()
	return cur == "" || versions.GreaterThanOrEqualTo(cur, v)
}

func tlsHTTPClient(t *config.TLS) (*http.Client, error) {
	cert, err := tls.LoadX509KeyPair(t.ClientCert, t.ClientKey)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	tlsConf := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: !t.ShouldVerify(), // #nosec G402 -- explicit per-host opt-in (tls.verify=false), warned loudly at startup
	}
	if t.CACert != "" {
		ca, err := os.ReadFile(t.CACert)
		if err != nil {
			return nil, fmt.Errorf("read ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("ca: no certs found")
		}
		tlsConf.RootCAs = pool
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:       tlsConf,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}}, nil
}

func (cs *Clients) List() []*Client {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]*Client(nil), cs.list...)
}

func (cs *Clients) Unreachable() []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]string, 0, len(cs.failed))
	for _, h := range cs.failed {
		out = append(out, h.Name)
	}
	return out
}

func (cs *Clients) Close() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, c := range cs.list {
		_ = c.api.Close()
	}
}

func (cs *Clients) Ping(ctx context.Context) error {
	for _, c := range cs.List() {
		if _, err := c.api.Ping(ctx, client.PingOptions{}); err != nil {
			return fmt.Errorf("host %s: %w", c.Name, err)
		}
	}
	return nil
}

type Container struct {
	ID          string
	Name        string
	Image       string
	ImageID     string
	Labels      map[string]string
	State       string
	NetworkMode string
}

func (c *Client) ListContainers(ctx context.Context, includeStopped bool) ([]Container, error) {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	list, err := c.api.ContainerList(ctx, client.ContainerListOptions{All: includeStopped})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(list.Items))
	for _, ct := range list.Items {
		name := ""
		if len(ct.Names) > 0 {
			name = strings.TrimPrefix(ct.Names[0], "/")
		}
		image := ct.Image
		if image != "" && image == ct.ImageID {
			if insp, err := c.api.ContainerInspect(ctx, ct.ID, client.ContainerInspectOptions{}); err == nil && insp.Container.Config != nil && insp.Container.Config.Image != "" {
				image = insp.Container.Config.Image
			}
		}
		out = append(out, Container{
			ID:          ct.ID,
			Name:        name,
			Image:       image,
			ImageID:     ct.ImageID,
			Labels:      ct.Labels,
			State:       string(ct.State),
			NetworkMode: ct.HostConfig.NetworkMode,
		})
	}
	return out, nil
}

type FullContainer struct {
	ID            string
	Name          string
	Config        *container.Config
	HostConfig    *container.HostConfig
	NetworkConfig *network.NetworkingConfig
	Image         string
	ImageID       string
	Running       bool
	Platform      string

	imagePlatform *ocispec.Platform
	imageManifest string
	macAddress    string
}

func (fc *FullContainer) MACAddress() string { return fc.macAddress }

func containerMACAddress(raw json.RawMessage) string {
	var v struct {
		Config struct {
			MacAddress string
		}
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return v.Config.MacAddress
}

func (fc *FullContainer) ImageManifestDigest() string { return fc.imageManifest }

func (c *Client) Inspect(ctx context.Context, id string) (*FullContainer, error) {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	res, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	insp := res.Container
	if insp.ID == "" || insp.Config == nil {
		return nil, fmt.Errorf("inspect %s: incomplete response", id)
	}
	preserveVolumeMounts(&insp)
	clearForContainerNetwork(insp.Config, insp.HostConfig)
	fc := &FullContainer{
		ID:            insp.ID,
		Name:          strings.TrimPrefix(insp.Name, "/"),
		Config:        insp.Config,
		HostConfig:    insp.HostConfig,
		NetworkConfig: createEndpoints(&insp, c.apiAtLeast("1.44")),
		Image:         insp.Config.Image,
		ImageID:       insp.Image,
		Running:       insp.State != nil && insp.State.Running,
	}
	if d := insp.ImageManifestDescriptor; d != nil {
		if d.Platform != nil {
			p := *d.Platform
			fc.imagePlatform = &p
		}
		fc.imageManifest = d.Digest.String()
	}
	if !c.apiAtLeast("1.44") {
		fc.macAddress = containerMACAddress(res.Raw)
	}
	return fc, nil
}

func (fc *FullContainer) EffectiveStopTimeout(updateSecs int) int {
	if fc.Config != nil && fc.Config.StopTimeout != nil && *fc.Config.StopTimeout > updateSecs {
		return *fc.Config.StopTimeout
	}
	return updateSecs
}

func preserveVolumeMounts(insp *container.InspectResponse) {
	if insp.HostConfig == nil {
		return
	}
	for i, m := range insp.HostConfig.Mounts {
		if m.Type != mount.TypeVolume || m.Source != "" {
			continue
		}
		for _, mp := range insp.Mounts {
			if mp.Type == mount.TypeVolume && mp.Destination == m.Target && mp.Name != "" {
				insp.HostConfig.Mounts[i].Source = mp.Name
				break
			}
		}
	}
	covered := map[string]bool{}
	coveredNames := map[string]bool{}
	for _, m := range insp.HostConfig.Mounts {
		covered[m.Target] = true
		if m.Type == mount.TypeVolume && m.Source != "" {
			coveredNames[m.Source] = true
		}
	}
	for _, b := range insp.HostConfig.Binds {
		parts := strings.Split(b, ":")
		if len(parts) >= 2 {
			covered[parts[1]] = true
			coveredNames[parts[0]] = true
		}
	}
	for _, m := range insp.Mounts {
		if m.Type != mount.TypeVolume || m.Name == "" || covered[m.Destination] || coveredNames[m.Name] {
			continue
		}
		insp.HostConfig.Mounts = append(insp.HostConfig.Mounts, mount.Mount{
			Type:     mount.TypeVolume,
			Source:   m.Name,
			Target:   m.Destination,
			ReadOnly: !m.RW,
		})
	}
}

func (c *Client) PullImage(ctx context.Context, ref string, auth *config.RegistryAuth) (string, error) {
	return c.PullImagePlatform(ctx, ref, auth, "")
}

func (c *Client) PullImagePlatform(ctx context.Context, ref string, auth *config.RegistryAuth, platform string) (string, error) {
	ctx, cancel := opCtx(ctx, pullTimeout)
	defer cancel()
	opts := client.ImagePullOptions{}
	if p := parsePlatform(strings.ToLower(platform)); p != nil {
		opts.Platforms = []ocispec.Platform{*p}
	}
	if auth != nil {
		encoded, err := encodeAuth(auth)
		if err != nil {
			return "", err
		}
		opts.RegistryAuth = encoded
	}
	rc, err := c.api.ImagePull(ctx, ref, opts)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	dec := json.NewDecoder(rc)
	for {
		var msg struct {
			ErrorMessage string `json:"error"`
			ErrorDetail  struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("pull stream: %w", err)
		}
		if msg.ErrorMessage != "" || msg.ErrorDetail.Message != "" {
			m := msg.ErrorDetail.Message
			if m == "" {
				m = msg.ErrorMessage
			}
			return "", fmt.Errorf("pull %s: %s", ref, m)
		}
	}
	insp, err := c.api.ImageInspect(ctx, ref)
	if err != nil {
		return "", err
	}
	return insp.ID, nil
}

func encodeAuth(a *config.RegistryAuth) (string, error) {
	user, pass, err := a.Credentials()
	if err != nil {
		return "", err
	}
	host, _ := a.Scope()
	cfg := registry.AuthConfig{
		Username:      user,
		Password:      pass,
		ServerAddress: host,
	}
	b, err := json.Marshal(cfg) // #nosec G117 -- X-Registry-Auth wire format required by the Docker API
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

func (c *Client) StopContainer(ctx context.Context, id string, timeoutSecs int) error {
	ctx, cancel := opCtx(ctx, time.Duration(timeoutSecs)*time.Second+opTimeout)
	defer cancel()
	t := timeoutSecs
	_, err := c.api.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &t})
	return err
}

func StopMayContinue(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	ctx, cancel := opCtx(ctx, bulkTimeout)
	defer cancel()
	_, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: false})
	return err
}

func (c *Client) RenameContainer(ctx context.Context, id, name string) error {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	_, err := c.api.ContainerRename(ctx, id, client.ContainerRenameOptions{NewName: name})
	return err
}

func (c *Client) StartContainer(ctx context.Context, id string) error {
	ctx, cancel := opCtx(ctx, bulkTimeout)
	defer cancel()
	_, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

func (c *Client) RestartContainer(ctx context.Context, id string, timeoutSecs int) error {
	ctx, cancel := opCtx(ctx, time.Duration(timeoutSecs)*time.Second+bulkTimeout)
	defer cancel()
	t := timeoutSecs
	_, err := c.api.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: &t})
	return err
}

func (c *Client) CreateContainer(ctx context.Context, name string, fc *FullContainer) (string, error) {
	ctx, cancel := opCtx(ctx, bulkTimeout)
	defer cancel()
	nc, later := splitEndpoints(fc.HostConfig, fc.NetworkConfig, c.apiAtLeast("1.44"))
	var platform *ocispec.Platform
	if fc.Platform != "" {
		if c.apiAtLeast("1.41") {
			platform = parsePlatform(fc.Platform)
		}
	}
	res, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:           fc.Config,
		HostConfig:       fc.HostConfig,
		NetworkingConfig: nc,
		Platform:         platform,
		Name:             name,
	})
	if err != nil {
		return "", err
	}
	for _, ep := range later {
		if _, err := c.api.NetworkConnect(ctx, ep.name, client.NetworkConnectOptions{Container: res.ID, EndpointConfig: ep.settings}); err != nil {
			return res.ID, fmt.Errorf("connect network %s: %w", ep.name, err)
		}
	}
	return res.ID, nil
}

func (c *Client) ImageTag(ctx context.Context, source, target string) error {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	_, err := c.api.ImageTag(ctx, client.ImageTagOptions{Source: source, Target: target})
	return err
}

const ComposeImageLabel = "com.docker.compose.image"

func (fc *FullContainer) PinComposeImage(imageID string) {
	if fc.Config == nil || imageID == "" {
		return
	}
	if _, ok := fc.Config.Labels[ComposeImageLabel]; ok {
		fc.Config.Labels[ComposeImageLabel] = imageID
	}
}

func (c *Client) RemoveImageIfDangling(ctx context.Context, imgID string) error {
	ctx, cancel := opCtx(ctx, bulkTimeout)
	defer cancel()
	insp, err := c.api.ImageInspect(ctx, imgID)
	if err != nil {
		return err
	}
	if insp.ID != imgID || len(insp.RepoTags) > 0 {
		return nil
	}
	_, err = c.api.ImageRemove(ctx, imgID, client.ImageRemoveOptions{Force: false, PruneChildren: true})
	return err
}

type ImageDefaults struct {
	Env          []string
	Cmd          []string
	Entrypoint   []string
	WorkingDir   string
	Healthcheck  *container.HealthConfig
	Labels       map[string]string
	User         string
	StopSignal   string
	ExposedPorts network.PortSet
	Volumes      map[string]struct{}
}

func (c *Client) ImageDefaults(ctx context.Context, ref string) (*ImageDefaults, error) {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	insp, err := c.api.ImageInspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	if insp.Config == nil {
		return &ImageDefaults{}, nil
	}
	var exposed network.PortSet
	if len(insp.Config.ExposedPorts) > 0 {
		exposed = make(network.PortSet, len(insp.Config.ExposedPorts))
		for p := range insp.Config.ExposedPorts {
			if port, err := network.ParsePort(p); err == nil {
				exposed[port] = struct{}{}
			}
		}
	}
	return &ImageDefaults{
		Env:          insp.Config.Env,
		Cmd:          insp.Config.Cmd,
		Entrypoint:   insp.Config.Entrypoint,
		WorkingDir:   insp.Config.WorkingDir,
		Healthcheck:  insp.Config.Healthcheck,
		Labels:       insp.Config.Labels,
		User:         insp.Config.User,
		StopSignal:   insp.Config.StopSignal,
		ExposedPorts: exposed,
		Volumes:      insp.Config.Volumes,
	}, nil
}

func (fc *FullContainer) StripImageDefaults(old *ImageDefaults) {
	if fc.Config == nil || old == nil {
		return
	}
	if stringSlicesEqual(fc.Config.Cmd, old.Cmd) {
		fc.Config.Cmd = nil
	}
	if stringSlicesEqual(fc.Config.Entrypoint, old.Entrypoint) {
		fc.Config.Entrypoint = nil
	}
	if fc.Config.WorkingDir == old.WorkingDir {
		fc.Config.WorkingDir = ""
	}
	if fc.Config.User == old.User {
		fc.Config.User = ""
	}
	if fc.Config.StopSignal == old.StopSignal {
		fc.Config.StopSignal = ""
	}
	if reflect.DeepEqual(fc.Config.Healthcheck, old.Healthcheck) {
		fc.Config.Healthcheck = nil
	}
	var portBindings network.PortMap
	if fc.HostConfig != nil {
		portBindings = fc.HostConfig.PortBindings
	}
	fc.Config.ExposedPorts = stripInheritedPorts(fc.Config.ExposedPorts, old.ExposedPorts, portBindings)
	fc.Config.Volumes = stripInheritedVolumes(fc.Config.Volumes, old.Volumes)
	fc.Config.Env = stripInheritedEntries(fc.Config.Env, old.Env)
	fc.Config.Labels = stripInheritedLabels(fc.Config.Labels, old.Labels)
}

func stripInheritedEntries(have, fromImage []string) []string {
	if len(fromImage) == 0 || len(have) == 0 {
		return have
	}
	img := make(map[string]struct{}, len(fromImage))
	for _, e := range fromImage {
		img[e] = struct{}{}
	}
	kept := make([]string, 0, len(have))
	for _, e := range have {
		if _, ok := img[e]; !ok {
			kept = append(kept, e)
		}
	}
	return kept
}

func stripInheritedLabels(have, fromImage map[string]string) map[string]string {
	if len(fromImage) == 0 || len(have) == 0 {
		return have
	}
	kept := make(map[string]string, len(have))
	for k, v := range have {
		if iv, ok := fromImage[k]; ok && iv == v {
			continue
		}
		kept[k] = v
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func stripInheritedPorts(have, fromImage network.PortSet, bindings network.PortMap) network.PortSet {
	if len(fromImage) == 0 || len(have) == 0 {
		return have
	}
	kept := make(network.PortSet, len(have))
	for p := range have {
		if _, inherited := fromImage[p]; inherited {
			if _, bound := bindings[p]; !bound {
				continue
			}
		}
		kept[p] = struct{}{}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func stripInheritedVolumes(have, fromImage map[string]struct{}) map[string]struct{} {
	if len(fromImage) == 0 || len(have) == 0 {
		return have
	}
	kept := make(map[string]struct{}, len(have))
	for path := range have {
		if _, inherited := fromImage[path]; inherited {
			continue
		}
		kept[path] = struct{}{}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	insp, err := c.api.ImageInspect(ctx, ref)
	if err != nil {
		return "", err
	}
	return insp.ID, nil
}

func (c *Client) ImageRepoDigests(ctx context.Context, ref string) ([]string, error) {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	insp, err := c.api.ImageInspect(ctx, ref)
	if err != nil {
		return nil, err
	}
	return insp.RepoDigests, nil
}

func (c *Client) ImageCreated(ctx context.Context, ref string) (time.Time, error) {
	ctx, cancel := opCtx(ctx, opTimeout)
	defer cancel()
	insp, err := c.api.ImageInspect(ctx, ref)
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339Nano, insp.Created)
	if err != nil {
		t, err = time.Parse(time.RFC3339, insp.Created)
		if err != nil {
			return time.Time{}, err
		}
	}
	return t, nil
}

func (c *Client) ContainerHealth(ctx context.Context, id string) (status string, running bool, err error) {
	st, err := c.ContainerState(ctx, id)
	if err != nil {
		return "", false, err
	}
	return st.Health, st.Running, nil
}

type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("exec exited %d", e.Code) }

const maxExecOutput = 64 * 1024

func (c *Client) ExecInContainer(ctx context.Context, id string, cmd, env []string) (string, error) {
	resp, err := c.api.ExecCreate(ctx, id, client.ExecCreateOptions{
		Cmd:          cmd,
		Env:          env,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", err
	}
	att, err := c.api.ExecAttach(ctx, resp.ID, client.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer att.Close()
	stop := context.AfterFunc(ctx, att.Close)
	defer stop()
	buf := &limitedBuffer{max: maxExecOutput}
	_, _ = stdcopy.StdCopy(buf, buf, att.Reader)
	out := buf.String()
	if err := ctx.Err(); err != nil {
		return out, fmt.Errorf("exec: %w", err)
	}
	insp, err := c.api.ExecInspect(ctx, resp.ID, client.ExecInspectOptions{})
	if err != nil {
		return out, err
	}
	if insp.ExitCode != 0 {
		return out, &ExitError{Code: insp.ExitCode}
	}
	return out, nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}
