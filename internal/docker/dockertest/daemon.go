package dockertest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client/pkg/versions"
)

type Image struct {
	ID           string
	OS           string
	Architecture string
	Variant      string
	Config       *container.Config
	CrashLoop    bool
	Created      string
	RepoDigests  []string
	Manifest     string
	Health       string
	Exit         func(cmd []string) (code int, output string, exit bool)
	Run          func(r RunRequest) (code int, stdout, stderr string, exit bool)
}

type RunRequest struct {
	Entrypoint []string
	Cmd        []string
	User       string
	Labels     map[string]string
	Mounts     []mount.Mount
}

type Login struct {
	Server, Username, Password string
}

type Call struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
	Header http.Header
}

type Daemon struct {
	t          testing.TB
	srv        *httptest.Server
	apiVersion string
	os, arch   string

	mu          sync.Mutex
	seq         int
	containers  map[string]*container.InspectResponse
	images      map[string]*Image
	tags        map[string]string
	remote      map[string][]*Image
	calls       []Call
	startErr    map[string]string
	badNetworks map[string]bool
	failNext    []injected
	logs        map[string]string
	slowStops   map[string]slowStop
	macs        map[string]string
	containerd  bool
	digests     map[string]string
	stdout      map[string]string
	bindSource  func(path string) bool
	logins      map[Login]bool
	pullAuth    map[string][]Login
}

type slowStop struct {
	after time.Duration
	begun chan struct{}
}

type injected struct {
	method, suffix, msg string
	retagRef, retagID   string
}

func New(t testing.TB, apiVersion string) *Daemon {
	t.Helper()
	d := &Daemon{
		t:           t,
		apiVersion:  apiVersion,
		os:          "linux",
		arch:        "amd64",
		containers:  map[string]*container.InspectResponse{},
		images:      map[string]*Image{},
		tags:        map[string]string{},
		digests:     map[string]string{},
		remote:      map[string][]*Image{},
		startErr:    map[string]string{},
		badNetworks: map[string]bool{},
		logs:        map[string]string{},
		slowStops:   map[string]slowStop{},
		macs:        map[string]string{},
		stdout:      map[string]string{},
		logins:      map[Login]bool{},
		pullAuth:    map[string][]Login{},
	}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *Daemon) SetArch(arch string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.arch = arch
}

func (d *Daemon) UseContainerdStore() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.containerd = true
}

func (d *Daemon) Endpoint() string {
	return "tcp://" + strings.TrimPrefix(d.srv.URL, "http://")
}

func (d *Daemon) AddImage(img Image, refs ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	stored := d.storeImage(img)
	for _, r := range refs {
		d.tags[r] = stored.ID
	}
	d.notePulledAs(stored)
}

func (d *Daemon) notePulledAs(img *Image) {
	for _, rd := range img.RepoDigests {
		d.digests[normalizeRepo(rd)] = img.ID
	}
}

func (d *Daemon) Publish(ref string, img Image) {
	d.mu.Lock()
	defer d.mu.Unlock()
	stored := d.storeImageRecord(img)
	for i, have := range d.remote[ref] {
		if have.OS == stored.OS && have.Architecture == stored.Architecture && have.Variant == stored.Variant {
			d.remote[ref][i] = stored
			return
		}
	}
	d.remote[ref] = append(d.remote[ref], stored)
}

func (d *Daemon) Run(name string, cfg *container.Config, hc *container.HostConfig, endpoints map[string]*network.EndpointSettings) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	req := container.CreateRequest{Config: cfg, HostConfig: hc}
	if endpoints != nil {
		req.NetworkingConfig = &network.NetworkingConfig{EndpointsConfig: endpoints}
	}
	ct, status, msg := d.create(name, req, "")
	if status != 0 {
		d.t.Fatalf("dockertest: run %s: %s", name, msg)
	}
	if status, msg := d.start(ct); status != 0 {
		d.t.Fatalf("dockertest: start %s: %s", name, msg)
	}
	return ct.ID
}

func (d *Daemon) Create(name string, cfg *container.Config, hc *container.HostConfig) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	ct, status, msg := d.create(name, container.CreateRequest{Config: cfg, HostConfig: hc}, "")
	if status != 0 {
		d.t.Fatalf("dockertest: create %s: %s", name, msg)
	}
	return ct.ID
}

func (d *Daemon) CheckBindSources(exists func(path string) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.bindSource = exists
}

func (d *Daemon) AcceptLogin(server, username, password string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logins[Login{Server: server, Username: username, Password: password}] = true
}

func (d *Daemon) RequirePullAuth(repo, username, password string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	repo = normalizeRepo(repo)
	d.pullAuth[repo] = append(d.pullAuth[repo], Login{Username: username, Password: password})
}

func (d *Daemon) SetMACAddress(ref, mac string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ct := d.find(ref); ct != nil {
		d.macs[ct.ID] = mac
	}
}

func (d *Daemon) FailNetwork(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.badNetworks[name] = true
}

func (d *Daemon) FailNextStart(name, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.startErr[name] = msg
}

func (d *Daemon) FailNext(method, suffix, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.failNext = append(d.failNext, injected{method: method, suffix: suffix, msg: msg})
}

func (d *Daemon) RetagOnNext(method, suffix, ref, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !strings.HasPrefix(id, "sha256:") {
		id = "sha256:" + id
	}
	d.failNext = append(d.failNext, injected{method: method, suffix: suffix, retagRef: ref, retagID: id})
}

func (d *Daemon) Stop(ref string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ct := d.find(ref); ct != nil {
		d.stop(ct)
	}
}

func (d *Daemon) SlowStop(ref string, dur time.Duration) <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	begun := make(chan struct{})
	if ct := d.find(ref); ct != nil {
		d.slowStops[ct.ID] = slowStop{after: dur, begun: begun}
	}
	return begun
}

func (d *Daemon) StopLater(ref string, dur time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ct := d.find(ref); ct != nil {
		d.stopAfter(ct.ID, dur, nil)
	}
}

func (d *Daemon) stopAfter(id string, dur time.Duration, done chan struct{}) {
	time.AfterFunc(dur, func() {
		d.mu.Lock()
		if ct := d.containers[id]; ct != nil && ct.State.Running {
			d.stop(ct)
		}
		d.mu.Unlock()
		if done != nil {
			close(done)
		}
	})
}

func (d *Daemon) serveSlowStop(w http.ResponseWriter, r *http.Request, path string) bool {
	ref := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/stop")
	d.mu.Lock()
	ct := d.find(ref)
	if ct == nil {
		d.mu.Unlock()
		return false
	}
	s, ok := d.slowStops[ct.ID]
	if !ok {
		d.mu.Unlock()
		return false
	}
	delete(d.slowStops, ct.ID)
	d.calls = append(d.calls, Call{Method: r.Method, Path: path, Query: r.URL.Query()})
	done := make(chan struct{})
	d.stopAfter(ct.ID, s.after, done)
	d.mu.Unlock()
	close(s.begun)
	select {
	case <-done:
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
	return true
}

func (d *Daemon) ImageID(ref string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if img := d.resolveImage(ref); img != nil {
		return img.ID
	}
	return ""
}

func (d *Daemon) DropImage(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !strings.HasPrefix(id, "sha256:") {
		id = "sha256:" + id
	}
	for _, t := range d.tagsOf(id) {
		delete(d.tags, t)
	}
	delete(d.images, id)
}

func (d *Daemon) HasImage(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !strings.HasPrefix(id, "sha256:") {
		id = "sha256:" + id
	}
	_, ok := d.images[id]
	return ok
}

func (d *Daemon) Inspect(ref string) (container.InspectResponse, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ct := d.find(ref)
	if ct == nil {
		return container.InspectResponse{}, false
	}
	return clone(ct), true
}

func (d *Daemon) Names() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, ct := range d.containers {
		out = append(out, strings.TrimPrefix(ct.Name, "/"))
	}
	sort.Strings(out)
	return out
}

func (d *Daemon) Calls(method, pathSuffix string) []Call {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Call
	for _, c := range d.calls {
		if (method == "" || c.Method == method) && strings.HasSuffix(c.Path, pathSuffix) {
			out = append(out, c)
		}
	}
	return out
}

func (d *Daemon) storeImage(img Image) *Image {
	stored := d.storeImageRecord(img)
	d.images[stored.ID] = stored
	return stored
}

func (d *Daemon) storeImageRecord(img Image) *Image {
	cp := img
	if cp.OS == "" {
		cp.OS = "linux"
	}
	if cp.Architecture == "" {
		cp.Architecture = d.arch
	}
	if cp.ID == "" {
		d.seq++
		cp.ID = "sha256:" + hexID("image", d.seq)
	}
	if !strings.HasPrefix(cp.ID, "sha256:") {
		cp.ID = "sha256:" + cp.ID
	}
	return &cp
}

func hexID(kind string, n int) string {
	sum := sha256.Sum256([]byte(kind + strconv.Itoa(n)))
	return hex.EncodeToString(sum[:])
}

var versionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)

func (d *Daemon) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	path := versionPrefix.ReplaceAllString(r.URL.Path, "")
	reqVersion := d.apiVersion
	if m := versionPrefix.FindString(r.URL.Path); m != "" {
		reqVersion = strings.TrimPrefix(m, "/v")
	}

	if strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/wait") && r.Method == http.MethodPost {
		d.wait(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/wait"))
		return
	}
	if strings.HasPrefix(path, "/containers/") && strings.HasSuffix(path, "/stop") && r.Method == http.MethodPost &&
		d.serveSlowStop(w, r, path) {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if path != "/_ping" {
		d.calls = append(d.calls, Call{Method: r.Method, Path: path, Query: r.URL.Query(), Body: body, Header: r.Header.Clone()})
	}
	for i, f := range d.failNext {
		if f.method == r.Method && strings.HasSuffix(path, f.suffix) {
			d.failNext = append(d.failNext[:i], d.failNext[i+1:]...)
			if f.retagRef != "" {
				d.tags[f.retagRef] = f.retagID
				break
			}
			writeError(w, http.StatusInternalServerError, f.msg)
			return
		}
	}

	switch {
	case path == "/_ping":
		w.Header().Set("Api-Version", d.apiVersion)
		w.Header().Set("Ostype", d.os)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	case path == "/version" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"Os": d.os, "Arch": d.arch, "ApiVersion": d.apiVersion, "Version": "test"})
	case path == "/containers/json" && r.Method == http.MethodGet:
		d.list(w)
	case path == "/containers/create" && r.Method == http.MethodPost:
		var req container.CreateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ct, status, msg := d.create(r.URL.Query().Get("name"), req, reqVersion)
		if status != 0 {
			writeError(w, status, msg)
			return
		}
		writeJSON(w, http.StatusCreated, container.CreateResponse{ID: ct.ID})
	case strings.HasPrefix(path, "/containers/"):
		d.containerOp(w, r, path, reqVersion)
	case strings.HasPrefix(path, "/networks/") && strings.HasSuffix(path, "/connect") && r.Method == http.MethodPost:
		d.connect(w, strings.TrimSuffix(strings.TrimPrefix(path, "/networks/"), "/connect"), body)
	case path == "/auth" && r.Method == http.MethodPost:
		d.login(w, body)
	case path == "/images/create" && r.Method == http.MethodPost:
		if msg := d.pullDenied(r.URL.Query(), r.Header.Get(registry.AuthHeader)); msg != "" {
			writeError(w, http.StatusInternalServerError, msg)
			return
		}
		d.pull(w, r.URL.Query())
	case path == "/images/json" && r.Method == http.MethodGet:
		d.imageList(w, r.URL.Query())
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json") && r.Method == http.MethodGet:
		d.imageInspect(w, strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json"))
	case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/tag") && r.Method == http.MethodPost:
		d.imageTag(w, strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/tag"), r.URL.Query())
	case strings.HasPrefix(path, "/images/") && r.Method == http.MethodDelete:
		d.imageRemove(w, strings.TrimPrefix(path, "/images/"), r.URL.Query())
	default:
		writeError(w, http.StatusNotFound, "dockertest: unsupported "+r.Method+" "+path)
	}
}

func (d *Daemon) containerOp(w http.ResponseWriter, r *http.Request, path, apiVersion string) {
	rest := strings.TrimPrefix(path, "/containers/")
	ref, op, _ := strings.Cut(rest, "/")
	ct := d.find(ref)
	if ct == nil {
		writeError(w, http.StatusNotFound, "No such container: "+ref)
		return
	}
	q := r.URL.Query()
	switch {
	case op == "json" && r.Method == http.MethodGet:
		if mac := d.macs[ct.ID]; mac != "" && versions.LessThan(apiVersion, "1.52") {
			writeJSON(w, http.StatusOK, withMACAddress(ct, mac))
			return
		}
		writeJSON(w, http.StatusOK, ct)
	case op == "start" && r.Method == http.MethodPost:
		if ct.State.Running {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if status, msg := d.start(ct); status != 0 {
			writeError(w, status, msg)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case op == "stop" && r.Method == http.MethodPost:
		d.stop(ct)
		w.WriteHeader(http.StatusNoContent)
	case op == "restart" && r.Method == http.MethodPost:
		d.stop(ct)
		if d.containers[ct.ID] == nil {
			writeError(w, http.StatusNotFound, "No such container: "+ref)
			return
		}
		if status, msg := d.start(ct); status != 0 {
			writeError(w, status, msg)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case op == "rename" && r.Method == http.MethodPost:
		name := q.Get("name")
		if other := d.byName(name); other != nil && other.ID != ct.ID {
			writeError(w, http.StatusConflict, fmt.Sprintf("Conflict. The container name %q is already in use", "/"+name))
			return
		}
		ct.Name = "/" + name
		w.WriteHeader(http.StatusNoContent)
	case op == "" && r.Method == http.MethodDelete:
		if ct.State.Running && q.Get("force") != "1" {
			writeError(w, http.StatusConflict, "container is running")
			return
		}
		delete(d.containers, ct.ID)
		delete(d.logs, ct.ID)
		delete(d.stdout, ct.ID)
		delete(d.macs, ct.ID)
		w.WriteHeader(http.StatusNoContent)
	case op == "logs" && r.Method == http.MethodGet:
		d.writeLogs(w, ct, q.Get("tail"))
	default:
		writeError(w, http.StatusNotFound, "dockertest: unsupported container op "+r.Method+" "+op)
	}
}

func (d *Daemon) find(ref string) *container.InspectResponse {
	ref = strings.TrimPrefix(ref, "/")
	if ct, ok := d.containers[ref]; ok {
		return ct
	}
	if ct := d.byName(ref); ct != nil {
		return ct
	}
	var found *container.InspectResponse
	for id, ct := range d.containers {
		if strings.HasPrefix(id, ref) {
			if found != nil {
				return nil
			}
			found = ct
		}
	}
	return found
}

func (d *Daemon) byName(name string) *container.InspectResponse {
	for _, ct := range d.containers {
		if ct.Name == "/"+name {
			return ct
		}
	}
	return nil
}

func (d *Daemon) resolveImage(ref string) *Image {
	if id, ok := d.tags[ref]; ok {
		return d.images[id]
	}
	if img, ok := d.images[ref]; ok {
		return img
	}
	if img, ok := d.images["sha256:"+ref]; ok {
		return img
	}
	if strings.Contains(ref, "@") {
		want := normalizeRepo(ref)
		if img := d.images[d.digests[want]]; img != nil {
			return img
		}
		for _, id := range d.sortedImageIDs() {
			if img := d.images[id]; slices.Contains(img.RepoDigests, want) {
				return img
			}
		}
	}
	return nil
}

func (d *Daemon) sortedImageIDs() []string {
	ids := make([]string, 0, len(d.images))
	for id := range d.images {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (d *Daemon) create(name string, req container.CreateRequest, apiVersion string) (*container.InspectResponse, int, string) {
	if name != "" && d.byName(name) != nil {
		return nil, http.StatusConflict, fmt.Sprintf("Conflict. The container name %q is already in use", "/"+name)
	}
	cfg := req.Config
	if cfg == nil {
		return nil, http.StatusBadRequest, "config is required"
	}
	hc := req.HostConfig
	if hc == nil {
		hc = &container.HostConfig{}
	}
	img := d.resolveImage(cfg.Image)
	if img == nil {
		return nil, http.StatusNotFound, "No such image: " + cfg.Image
	}
	if d.bindSource != nil {
		for _, m := range hc.Mounts {
			if m.Type == mount.TypeBind && !d.bindSource(m.Source) {
				return nil, http.StatusBadRequest, `invalid mount config for type "bind": bind source path does not exist: ` + m.Source
			}
		}
	}
	var eps map[string]*network.EndpointSettings
	if req.NetworkingConfig != nil {
		eps = req.NetworkingConfig.EndpointsConfig
	}
	if hc.NetworkMode.IsContainer() {
		switch {
		case cfg.Hostname != "":
			return nil, http.StatusBadRequest, "conflicting options: hostname and the network mode"
		case len(hc.DNS) > 0:
			return nil, http.StatusBadRequest, "conflicting options: dns and the network mode"
		case len(hc.ExtraHosts) > 0:
			return nil, http.StatusBadRequest, "conflicting options: custom host-to-IP mapping and the network mode"
		case len(hc.Links) > 0:
			return nil, http.StatusBadRequest, "conflicting options: container type network can't be used with links"
		case len(hc.PortBindings) > 0 || hc.PublishAllPorts:
			return nil, http.StatusBadRequest, "conflicting options: port publishing and the container type network mode"
		case len(cfg.ExposedPorts) > 0:
			return nil, http.StatusBadRequest, "conflicting options: port exposing and the container type network mode"
		}
		if d.find(hc.NetworkMode.ConnectedContainer()) == nil {
			return nil, http.StatusNotFound, "No such container: " + hc.NetworkMode.ConnectedContainer()
		}
	}
	if apiVersion == "" {
		apiVersion = d.apiVersion
	} else if versions.LessThan(apiVersion, "1.44") && len(eps) > 1 {
		names := make([]string, 0, len(eps))
		for n := range eps {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, http.StatusBadRequest, "Container cannot be connected to network endpoints: " + strings.Join(names, ", ")
	}

	d.seq++
	id := hexID("container", d.seq)
	merged := mergeImageConfig(*cfg, img.Config)
	if merged.Hostname == "" && !hc.NetworkMode.IsContainer() {
		merged.Hostname = id[:12]
	}
	ct := &container.InspectResponse{
		ID:              id,
		Created:         time.Now().UTC().Format(time.RFC3339Nano),
		Name:            "/" + name,
		Image:           img.ID,
		Platform:        img.OS,
		HostConfig:      hc,
		State:           &container.State{Status: container.StateCreated, StartedAt: "0001-01-01T00:00:00Z"},
		Config:          &merged,
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{}},
	}
	if img.Manifest != "" {
		desc, _ := json.Marshal(map[string]any{
			"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": img.Manifest, "size": 1,
			"platform": map[string]string{"os": img.OS, "architecture": img.Architecture, "variant": img.Variant},
		})
		_ = json.Unmarshal(desc, &ct.ImageManifestDescriptor)
	}
	ct.Mounts = d.mounts(hc, merged.Volumes)
	if !hc.NetworkMode.IsContainer() {
		if len(eps) == 0 {
			netName := string(hc.NetworkMode)
			if netName == "" || hc.NetworkMode.IsDefault() {
				netName = network.NetworkBridge
			}
			eps = map[string]*network.EndpointSettings{netName: {}}
		}
		for n, ep := range eps {
			ct.NetworkSettings.Networks[n] = d.endpoint(n, id, ep, apiVersion)
		}
	}
	d.containers[id] = ct
	return ct, 0, ""
}

func (d *Daemon) mounts(hc *container.HostConfig, volumes map[string]struct{}) []container.MountPoint {
	var out []container.MountPoint
	covered := map[string]bool{}
	for _, m := range hc.Mounts {
		covered[m.Target] = true
		switch m.Type {
		case mount.TypeVolume:
			name := m.Source
			if name == "" {
				d.seq++
				name = hexID("volume", d.seq)
			}
			out = append(out, container.MountPoint{Type: mount.TypeVolume, Name: name, Destination: m.Target, RW: !m.ReadOnly})
		case mount.TypeBind:
			out = append(out, container.MountPoint{Type: mount.TypeBind, Source: m.Source, Destination: m.Target, RW: !m.ReadOnly})
		}
	}
	for _, b := range hc.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 2 {
			continue
		}
		covered[parts[1]] = true
		if strings.HasPrefix(parts[0], "/") {
			out = append(out, container.MountPoint{Type: mount.TypeBind, Source: parts[0], Destination: parts[1], RW: true})
		} else {
			out = append(out, container.MountPoint{Type: mount.TypeVolume, Name: parts[0], Destination: parts[1], RW: true})
		}
	}
	dests := make([]string, 0, len(volumes))
	for dst := range volumes {
		dests = append(dests, dst)
	}
	sort.Strings(dests)
	for _, dst := range dests {
		if covered[dst] {
			continue
		}
		d.seq++
		out = append(out, container.MountPoint{Type: mount.TypeVolume, Name: hexID("volume", d.seq), Destination: dst, RW: true})
	}
	return out
}

func (d *Daemon) endpoint(netName, id string, cfg *network.EndpointSettings, apiVersion string) *network.EndpointSettings {
	ep := &network.EndpointSettings{}
	if cfg != nil {
		cp := *cfg
		ep = &cp
	}
	d.seq++
	ep.NetworkID = hexID("network:"+netName, 0)
	ep.EndpointID = hexID("endpoint", d.seq)
	ep.IPAddress = netip.MustParseAddr(fmt.Sprintf("172.18.%d.%d", d.seq/250, d.seq%250+2))
	if len(ep.MacAddress) == 0 {
		mac, _ := net.ParseMAC(fmt.Sprintf("02:42:ac:12:%02x:%02x", d.seq/250, d.seq%250+2))
		ep.MacAddress = network.HardwareAddr(mac)
	}
	user := netName != network.NetworkBridge && netName != network.NetworkHost && netName != network.NetworkNone
	if user && versions.LessThan(apiVersion, "1.45") {
		ep.Aliases = append(append([]string(nil), ep.Aliases...), id[:12])
	}
	return ep
}

func (d *Daemon) start(ct *container.InspectResponse) (int, string) {
	name := strings.TrimPrefix(ct.Name, "/")
	if msg, ok := d.startErr[name]; ok {
		delete(d.startErr, name)
		return http.StatusInternalServerError, msg
	}
	if ct.HostConfig.NetworkMode.IsContainer() {
		owner := d.find(ct.HostConfig.NetworkMode.ConnectedContainer())
		if owner == nil {
			return http.StatusNotFound, "No such container: " + ct.HostConfig.NetworkMode.ConnectedContainer()
		}
		if !owner.State.Running {
			return http.StatusConflict, "cannot join network namespace of a non running container"
		}
		ct.Config.Hostname = owner.Config.Hostname
		ct.Config.Domainname = owner.Config.Domainname
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ct.State.StartedAt = now
	img := d.images[ct.Image]
	if img != nil && img.CrashLoop {
		ct.State.ExitCode = 1
		if p := ct.HostConfig.RestartPolicy.Name; p != "" && p != container.RestartPolicyDisabled {
			ct.State.Status = container.StateRestarting
			ct.State.Running = true
			ct.State.Restarting = true
			ct.RestartCount++
			return 0, ""
		}
		ct.State.Status = container.StateExited
		ct.State.Running = false
		ct.State.FinishedAt = now
		return 0, ""
	}
	if img != nil && img.Run != nil {
		code, stdout, stderr, exit := img.Run(RunRequest{
			Entrypoint: slices.Clone(ct.Config.Entrypoint), Cmd: slices.Clone(ct.Config.Cmd),
			User: ct.Config.User, Labels: ct.Config.Labels, Mounts: slices.Clone(ct.HostConfig.Mounts),
		})
		if exit {
			ct.State.Status = container.StateExited
			ct.State.Running = false
			ct.State.ExitCode = code
			ct.State.FinishedAt = now
			d.stdout[ct.ID] = stdout
			d.logs[ct.ID] = stderr
			return 0, ""
		}
	}
	if img != nil && img.Exit != nil {
		if code, out, exit := img.Exit(append([]string(nil), ct.Config.Cmd...)); exit {
			ct.State.Status = container.StateExited
			ct.State.Running = false
			ct.State.ExitCode = code
			ct.State.FinishedAt = now
			d.logs[ct.ID] = out
			return 0, ""
		}
	}
	ct.State.Status = container.StateRunning
	ct.State.Running = true
	ct.State.Restarting = false
	ct.State.ExitCode = 0
	ct.State.Pid = 1000 + d.seq
	ct.State.Health = nil
	if img != nil && img.Health != "" {
		ct.State.Health = &container.Health{Status: container.HealthStatus(img.Health)}
	}
	return 0, ""
}

func (d *Daemon) stop(ct *container.InspectResponse) {
	ct.State.Running = false
	ct.State.Restarting = false
	ct.State.Status = container.StateExited
	ct.State.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if ct.HostConfig.AutoRemove {
		delete(d.containers, ct.ID)
	}
}

func (d *Daemon) connect(w http.ResponseWriter, netName string, body []byte) {
	if d.badNetworks[netName] {
		writeError(w, http.StatusNotFound, "network "+netName+" not found")
		return
	}
	var req network.ConnectRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ct := d.find(req.Container)
	if ct == nil {
		writeError(w, http.StatusNotFound, "No such container: "+req.Container)
		return
	}
	if ct.HostConfig.NetworkMode.IsContainer() {
		writeError(w, http.StatusBadRequest, "container sharing network namespace with another container cannot be connected to any other network")
		return
	}
	ct.NetworkSettings.Networks[netName] = d.endpoint(netName, ct.ID, req.EndpointConfig, d.apiVersion)
	w.WriteHeader(http.StatusOK)
}

func (d *Daemon) pull(w http.ResponseWriter, q url.Values) {
	ref := q.Get("fromImage")
	if tag := q.Get("tag"); strings.HasPrefix(tag, "sha256:") || strings.HasPrefix(tag, "sha512:") {
		d.pullDigest(w, normalizeRepo(ref)+"@"+tag, q.Get("platform"))
		return
	} else if tag != "" {
		ref += ":" + tag
	}
	imgs := d.remote[ref]
	if len(imgs) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"errorDetail": map[string]string{"message": "manifest unknown"}, "error": "manifest unknown"})
		return
	}
	want := q.Get("platform")
	if want == "" {
		want = d.os + "/" + d.arch
	}
	var pick *Image
	for _, img := range imgs {
		p := img.OS + "/" + img.Architecture
		if p == want || (img.Variant != "" && p+"/"+img.Variant == want) {
			pick = img
			break
		}
	}
	if pick == nil {
		writeJSON(w, http.StatusOK, map[string]any{"errorDetail": map[string]string{"message": "no matching manifest for " + want}, "error": "no matching manifest"})
		return
	}
	d.images[pick.ID] = pick
	d.tags[ref] = pick.ID
	d.notePulledAs(pick)
	writeJSON(w, http.StatusOK, map[string]string{"status": "Downloaded newer image for " + ref})
}

func (d *Daemon) pullDigest(w http.ResponseWriter, ref, platform string) {
	if platform == "" {
		platform = d.os + "/" + d.arch
	}
	refs := make([]string, 0, len(d.remote))
	for r := range d.remote {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	var named bool
	for _, r := range refs {
		for _, img := range d.remote[r] {
			if !slices.Contains(img.RepoDigests, ref) {
				continue
			}
			named = true
			p := img.OS + "/" + img.Architecture
			if p != platform && (img.Variant == "" || p+"/"+img.Variant != platform) {
				continue
			}
			d.images[img.ID] = img
			d.digests[ref] = img.ID
			if d.containerd {
				d.tags[ref] = img.ID
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "Downloaded newer image for " + ref})
			return
		}
	}
	msg := "manifest unknown"
	if named {
		msg = "no matching manifest for " + platform
	}
	writeJSON(w, http.StatusOK, map[string]any{"errorDetail": map[string]string{"message": msg}, "error": msg})
}

func (d *Daemon) imageInspect(w http.ResponseWriter, ref string) {
	img := d.resolveImage(ref)
	if img == nil {
		writeError(w, http.StatusNotFound, "No such image: "+ref)
		return
	}
	cfg := img.Config
	if cfg == nil {
		cfg = &container.Config{}
	}
	created := img.Created
	if created == "" {
		created = "2024-01-01T00:00:00Z"
	}
	var tags []string
	digests := slices.Clone(img.RepoDigests)
	for _, name := range d.tagsOf(img.ID) {
		if strings.Contains(name, "@") {
			if !slices.Contains(digests, name) {
				digests = append(digests, name)
			}
			continue
		}
		tags = append(tags, name)
	}
	if tags == nil {
		tags = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"Id":           img.ID,
		"RepoTags":     tags,
		"RepoDigests":  digests,
		"Created":      created,
		"Os":           img.OS,
		"Architecture": img.Architecture,
		"Variant":      img.Variant,
		"Config":       cfg,
	})
}

func (d *Daemon) wait(w http.ResponseWriter, r *http.Request, ref string) {
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: r.Method, Path: "/containers/" + ref + "/wait", Query: r.URL.Query()})
	d.mu.Unlock()
	for {
		d.mu.Lock()
		ct := d.find(ref)
		if ct == nil {
			d.mu.Unlock()
			writeError(w, http.StatusNotFound, "No such container: "+ref)
			return
		}
		if !ct.State.Running {
			code := ct.State.ExitCode
			d.mu.Unlock()
			writeJSON(w, http.StatusOK, container.WaitResponse{StatusCode: int64(code)})
			return
		}
		d.mu.Unlock()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Millisecond):
		}
	}
}

func (d *Daemon) writeLogs(w http.ResponseWriter, ct *container.InspectResponse, tail string) {
	out := d.logs[ct.ID]
	if n, err := strconv.Atoi(tail); err == nil && n >= 0 {
		lines := strings.SplitAfter(out, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		out = strings.Join(lines, "")
	}
	w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	w.WriteHeader(http.StatusOK)
	writeFrames(w, stdcopy.Stdout, d.stdout[ct.ID])
	writeFrames(w, stdcopy.Stderr, out)
}

func writeFrames(w io.Writer, stream stdcopy.StdType, out string) {
	for len(out) > 0 {
		n := min(len(out), 1<<16)
		frame := binary.BigEndian.AppendUint32([]byte{byte(stream), 0, 0, 0}, uint32(n))
		_, _ = w.Write(append(frame, out[:n]...))
		out = out[n:]
	}
}

func (d *Daemon) login(w http.ResponseWriter, body []byte) {
	var ac registry.AuthConfig
	if err := json.Unmarshal(body, &ac); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !d.logins[Login{Server: ac.ServerAddress, Username: ac.Username, Password: ac.Password}] {
		writeError(w, http.StatusUnauthorized, "login attempt to https://"+ac.ServerAddress+"/v2/ failed with status: 401 Unauthorized")
		return
	}
	writeJSON(w, http.StatusOK, registry.AuthResponse{Status: "Login Succeeded"})
}

func (d *Daemon) pullDenied(q url.Values, header string) string {
	repo := normalizeRepo(q.Get("fromImage"))
	want, ok := d.pullAuth[repo]
	if !ok {
		return ""
	}
	var ac registry.AuthConfig
	if raw, err := base64.URLEncoding.DecodeString(header); err == nil {
		_ = json.Unmarshal(raw, &ac)
	}
	for _, l := range want {
		if l.Username == ac.Username && l.Password == ac.Password {
			return ""
		}
	}
	return "Head \"https://" + repo + "/manifests/" + q.Get("tag") + "\": unauthorized: authentication required"
}

func normalizeRepo(repo string) string {
	repo = strings.TrimPrefix(repo, "docker.io/")
	return strings.TrimPrefix(repo, "library/")
}

func (d *Daemon) tagsOf(id string) []string {
	out := []string{}
	for ref, have := range d.tags {
		if have == id {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

func (d *Daemon) imageTag(w http.ResponseWriter, source string, q url.Values) {
	img := d.resolveImage(source)
	if img == nil {
		writeError(w, http.StatusNotFound, "No such image: "+source)
		return
	}
	tag := q.Get("tag")
	if tag == "" {
		tag = "latest"
	}
	d.tags[normalizeRepo(q.Get("repo"))+":"+tag] = img.ID
	w.WriteHeader(http.StatusCreated)
}

func (d *Daemon) inUse(id string) string {
	for _, ct := range d.containers {
		if ct.Image == id {
			return ct.ID
		}
	}
	return ""
}

func (d *Daemon) imageRemove(w http.ResponseWriter, ref string, q url.Values) {
	ref = normalizeRepo(ref)
	var resp []map[string]string
	id, tagged := d.tags[ref]
	if tagged {
		delete(d.tags, ref)
		resp = append(resp, map[string]string{"Untagged": ref})
		if len(d.tagsOf(id)) > 0 || d.inUse(id) != "" {
			writeJSON(w, http.StatusOK, resp)
			return
		}
	} else {
		img := d.resolveImage(ref)
		if img == nil {
			writeError(w, http.StatusNotFound, "No such image: "+ref)
			return
		}
		id = img.ID
		if c := d.inUse(id); c != "" {
			writeError(w, http.StatusConflict, fmt.Sprintf("conflict: unable to delete %s - image is being used by container %s", short(id, 19), short(c, 12)))
			return
		}
		tags := d.tagsOf(id)
		if len(tags) > 1 && q.Get("force") != "1" {
			writeError(w, http.StatusConflict, fmt.Sprintf("conflict: unable to delete %s (must be forced) - image is referenced in multiple repositories", short(id, 19)))
			return
		}
		for _, t := range tags {
			delete(d.tags, t)
			resp = append(resp, map[string]string{"Untagged": t})
		}
	}
	delete(d.images, id)
	resp = append(resp, map[string]string{"Deleted": id})
	writeJSON(w, http.StatusOK, resp)
}

func short(s string, n int) string {
	return s[:min(n, len(s))]
}

func (d *Daemon) imageList(w http.ResponseWriter, q url.Values) {
	danglingOnly := strings.Contains(q.Get("filters"), "dangling")
	ids := make([]string, 0, len(d.images))
	for id := range d.images {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []map[string]any{}
	for _, id := range ids {
		tags := d.tagsOf(id)
		if danglingOnly && len(tags) > 0 {
			continue
		}
		out = append(out, map[string]any{"Id": id, "RepoTags": tags})
	}
	writeJSON(w, http.StatusOK, out)
}

func (d *Daemon) list(w http.ResponseWriter) {
	ids := make([]string, 0, len(d.containers))
	for id := range d.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]container.Summary, 0, len(ids))
	for _, id := range ids {
		ct := d.containers[id]
		image := ct.Config.Image
		if img := d.resolveImage(image); img == nil || img.ID != ct.Image {
			image = ct.Image
		}
		s := container.Summary{
			ID:      ct.ID,
			Names:   []string{ct.Name},
			Image:   image,
			ImageID: ct.Image,
			Labels:  ct.Config.Labels,
			State:   ct.State.Status,
		}
		s.HostConfig.NetworkMode = string(ct.HostConfig.NetworkMode)
		out = append(out, s)
	}
	writeJSON(w, http.StatusOK, out)
}

func mergeImageConfig(cfg container.Config, img *container.Config) container.Config {
	if img == nil {
		return cfg
	}
	if len(cfg.Cmd) == 0 && len(cfg.Entrypoint) == 0 {
		cfg.Cmd = img.Cmd
	}
	if len(cfg.Entrypoint) == 0 {
		cfg.Entrypoint = img.Entrypoint
	}
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = img.WorkingDir
	}
	if cfg.User == "" {
		cfg.User = img.User
	}
	if cfg.StopSignal == "" {
		cfg.StopSignal = img.StopSignal
	}
	if cfg.Healthcheck == nil {
		cfg.Healthcheck = img.Healthcheck
	}
	have := map[string]bool{}
	for _, e := range cfg.Env {
		k, _, _ := strings.Cut(e, "=")
		have[k] = true
	}
	var env []string
	for _, e := range img.Env {
		if k, _, _ := strings.Cut(e, "="); !have[k] {
			env = append(env, e)
		}
	}
	cfg.Env = append(env, cfg.Env...)
	if len(img.Labels) > 0 {
		labels := map[string]string{}
		for k, v := range img.Labels {
			labels[k] = v
		}
		for k, v := range cfg.Labels {
			labels[k] = v
		}
		cfg.Labels = labels
	}
	if len(img.Volumes) > 0 {
		vols := map[string]struct{}{}
		for k := range img.Volumes {
			vols[k] = struct{}{}
		}
		for k := range cfg.Volumes {
			vols[k] = struct{}{}
		}
		cfg.Volumes = vols
	}
	return cfg
}

func withMACAddress(ct *container.InspectResponse, mac string) map[string]any {
	b, _ := json.Marshal(ct)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	if cfg, ok := out["Config"].(map[string]any); ok {
		cfg["MacAddress"] = mac
	}
	return out
}

func clone(ct *container.InspectResponse) container.InspectResponse {
	b, _ := json.Marshal(ct)
	var out container.InspectResponse
	_ = json.Unmarshal(b, &out)
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}
