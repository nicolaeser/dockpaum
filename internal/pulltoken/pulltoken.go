package pulltoken

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	cerrdefs "github.com/containerd/errdefs"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/policy"
	"github.com/nicolaeser/dockpaum/internal/registry"
)

const (
	FileName        = ".pull_token"
	WorkingDirLabel = "com.docker.compose.project.working_dir"
	MaxFileSize     = 64 << 10
)

func ProjectDir(labels map[string]string) string {
	return strings.TrimSpace(labels[WorkingDirLabel])
}

type Options struct {
	Logger      *slog.Logger
	Instance    string
	InContainer bool
	Local       func(host string) bool
}

type Self struct {
	ID      string
	Clients []*docker.Client
}

type File struct {
	Path        string
	Credentials []registry.Credential
	Problem     string
}

type record struct {
	file    *File
	sum     [sha256.Size]byte
	parsed  bool
	invalid string
	readErr string
}

type Store struct {
	opts  Options
	log   *slog.Logger
	read  func(path string) ([]byte, error)
	login func(ctx context.Context, c *docker.Client, a *config.RegistryAuth) error

	mu        sync.Mutex
	selfID    string
	selfImage string
	selfRef   string
	files     map[string]*record
	notices   map[string]string
}

func New(o Options) *Store {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Local == nil {
		o.Local = func(string) bool { return false }
	}
	return &Store{
		opts: o,
		log:  o.Logger,
		read: readLocal,
		login: func(ctx context.Context, c *docker.Client, a *config.RegistryAuth) error {
			return c.RegistryLogin(ctx, a)
		},
		files:   map[string]*record{},
		notices: map[string]string{},
	}
}

func (s *Store) Load(ctx context.Context, c *docker.Client, self Self, all []docker.Container, dirs []string) map[string]*File {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeReaders(ctx, c, all)
	s.forgetUnused(c.Name, all)
	dirs = uniqueDirs(dirs)
	if len(dirs) == 0 {
		return nil
	}
	results, reason := s.readAll(ctx, c, self, dirs)
	if results == nil {
		if s.notices[c.Name] != reason {
			s.notices[c.Name] = reason
			s.log.Warn("cannot read "+FileName+" files on this Docker host; the credentials read from them before are kept, other containers use the registry entries of the config",
				"host", c.Name, "reason", reason)
		}
		return s.cached(c.Name, dirs)
	}
	if _, ok := s.notices[c.Name]; ok {
		delete(s.notices, c.Name)
		s.log.Info(FileName+" files can be read on this Docker host again", "host", c.Name)
	}
	out := map[string]*File{}
	for _, dir := range dirs {
		if f := s.apply(ctx, c, dir, results[dir]); f != nil {
			out[dir] = f
		}
	}
	return out
}

func uniqueDirs(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

func fileKey(host, dir string) string { return host + "\x00" + dir }

func (s *Store) forgetUnused(host string, all []docker.Container) {
	used := map[string]bool{}
	for _, ct := range all {
		if dir := ProjectDir(ct.Labels); dir != "" {
			used[fileKey(host, dir)] = true
		}
	}
	for key := range s.files {
		if strings.HasPrefix(key, host+"\x00") && !used[key] {
			delete(s.files, key)
		}
	}
}

func (s *Store) removeReaders(ctx context.Context, c *docker.Client, all []docker.Container) {
	for _, ct := range all {
		if !docker.IsFileReader(ct) {
			continue
		}
		switch {
		case ct.State == "exited" || ct.State == "dead":
		case ct.State == "created" && ct.Labels[docker.FileReaderLabel] == s.opts.Instance:
		default:
			continue
		}
		if err := c.RemoveOneShot(ctx, ct.ID); err != nil {
			s.log.Debug("removing a leftover file reader container failed", "host", c.Name, "container", ct.Name, "err", err)
		}
	}
}

func (s *Store) cached(host string, dirs []string) map[string]*File {
	out := map[string]*File{}
	for _, dir := range dirs {
		if r := s.files[fileKey(host, dir)]; r != nil && r.file != nil {
			out[dir] = r.file
		}
	}
	return out
}

func (s *Store) readAll(ctx context.Context, c *docker.Client, self Self, dirs []string) (map[string]docker.HostFile, string) {
	if !s.opts.InContainer {
		if !s.opts.Local(c.Name) {
			return nil, "dockpaum runs outside a container and this Docker host is not on this machine"
		}
		out := make(map[string]docker.HostFile, len(dirs))
		for _, dir := range dirs {
			data, err := s.read(filepath.Join(dir, FileName))
			out[dir] = docker.HostFile{Data: data, Err: err}
		}
		return out, ""
	}
	image, reason := s.imageOn(ctx, c, self)
	if image == "" {
		return nil, reason
	}
	out, err := c.ReadHostFiles(ctx, docker.FileReader{
		Image: image, Name: FileName, MaxSize: MaxFileSize,
		Labels: map[string]string{docker.FileReaderLabel: s.opts.Instance, policy.LabelEnable: "false"},
	}, dirs)
	if err != nil {
		return nil, err.Error()
	}
	return out, ""
}

func (s *Store) imageOn(ctx context.Context, c *docker.Client, self Self) (string, string) {
	if self.ID == "" {
		return "", "dockpaum's own container was not found on the Docker hosts, so its image cannot read the files"
	}
	if s.selfID != self.ID || s.selfImage == "" {
		s.selfID, s.selfImage, s.selfRef = self.ID, "", ""
		for _, cl := range self.Clients {
			if fc, err := cl.Inspect(ctx, self.ID); err == nil && fc.ImageID != "" {
				s.selfImage, s.selfRef = fc.ImageID, fc.Image
				break
			}
		}
		if s.selfImage == "" {
			return "", "dockpaum's own container could not be inspected"
		}
	}
	for _, ref := range []string{s.selfImage, s.selfRef} {
		if ref == "" {
			continue
		}
		if _, err := c.ImageID(ctx, ref); err == nil {
			return ref, ""
		} else if !cerrdefs.IsNotFound(err) {
			return "", "checking for dockpaum's image failed: " + err.Error()
		}
	}
	return "", "dockpaum's image is not present on this Docker host"
}

func (s *Store) apply(ctx context.Context, c *docker.Client, dir string, res docker.HostFile) *File {
	key := fileKey(c.Name, dir)
	path := docker.HostPath(dir, FileName)
	logger := s.log.With("host", c.Name, "file", path)
	rec := s.files[key]

	switch {
	case errors.Is(res.Err, fs.ErrNotExist):
		if rec != nil && rec.parsed {
			logger.Info(FileName + " removed; the project's containers use the registry entries of the config again")
		}
		delete(s.files, key)
		return nil
	case res.Err != nil:
		if rec == nil {
			rec = &record{}
			s.files[key] = rec
		}
		msg := res.Err.Error()
		if rec.readErr != msg {
			rec.readErr = msg
			logger.Warn("cannot read "+FileName+"; the credentials read from it before are kept, if any", "err", msg)
		}
		var creds []registry.Credential
		if rec.file != nil {
			creds = rec.file.Credentials
		}
		problem := fmt.Sprintf("%s cannot be read: %s", path, msg)
		if len(creds) > 0 {
			problem += "; the credentials read from it before are used"
		}
		rec.file = &File{Path: path, Credentials: creds, Problem: problem}
		return rec.file
	}

	sum := sha256.Sum256(res.Data)
	if rec != nil && rec.parsed && rec.sum == sum {
		if rec.readErr != "" {
			rec.readErr = ""
			rec.file = &File{Path: path, Credentials: rec.file.Credentials, Problem: rec.invalid}
		}
		return rec.file
	}
	changed := rec != nil && rec.parsed
	entries, lineErrs := Parse(res.Data)
	f := &File{Path: path}
	for _, e := range entries {
		f.Credentials = append(f.Credentials, registry.NewCredential(e.Auth, fmt.Sprintf("%s line %d", path, e.Line)))
	}
	var invalid []string
	for _, le := range lineErrs {
		invalid = append(invalid, le.Error())
	}
	if len(invalid) > 0 {
		f.Problem = fmt.Sprintf("invalid lines in %s are ignored: %s", path, strings.Join(invalid, "; "))
	}
	s.files[key] = &record{file: f, sum: sum, parsed: true, invalid: f.Problem}

	msg := FileName + " loaded; the project's containers pull with its credentials"
	if changed {
		msg = FileName + " changed; the project's containers pull with its new credentials"
	}
	logger.Info(msg, "entries", len(entries), "scopes", scopes(f.Credentials))
	for _, le := range lineErrs {
		logger.Warn("invalid line in "+FileName+"; the line is ignored", "line", le.Line, "reason", le.Reason)
	}
	s.checkLogins(ctx, c, logger, entries, f.Credentials)
	return f
}

func scopes(creds []registry.Credential) []string {
	var out []string
	for _, c := range creds {
		if sc := c.Scope(); !slices.Contains(out, sc) {
			out = append(out, sc)
		}
	}
	slices.Sort(out)
	return out
}

func (s *Store) checkLogins(ctx context.Context, c *docker.Client, logger *slog.Logger, entries []Entry, creds []registry.Credential) {
	type login struct{ server, user, pass string }
	done := map[login]bool{}
	for i, cred := range creds {
		host, _ := cred.Auth.Scope()
		key := login{host, cred.Auth.Username, cred.Auth.Password}
		if done[key] {
			continue
		}
		done[key] = true
		a := cred.Auth
		if err := s.login(ctx, c, &a); err != nil {
			if docker.IsDenied(err) {
				cred.MarkReported()
			}
			logger.Warn("registry login with a "+FileName+" entry failed; pulls with it fail until it is fixed",
				"line", entries[i].Line, "scope", cred.Scope(), "username", cred.Auth.Username, "err", err)
			continue
		}
		logger.Info("registry login with a "+FileName+" entry succeeded",
			"line", entries[i].Line, "scope", cred.Scope(), "username", cred.Auth.Username)
	}
}

func readLocal(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, plainErr(err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if info.Size() > MaxFileSize {
		return nil, fmt.Errorf("larger than the %d bytes allowed", MaxFileSize)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the project directory comes from the compose working_dir label of a managed container
	if err != nil {
		return nil, plainErr(err)
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("larger than the %d bytes allowed", MaxFileSize)
	}
	return data, nil
}

func plainErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
