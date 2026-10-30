package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nicolaeser/dockpaum/internal/config"
)

var ErrDenied = errors.New("registry: credentials rejected")

type deniedError struct{ err error }

func (e *deniedError) Error() string { return e.err.Error() }

func (e *deniedError) Unwrap() error { return e.err }

func (e *deniedError) Is(target error) bool { return target == ErrDenied }

func denied(err error) error { return &deniedError{err: err} }

type Credential struct {
	Auth   config.RegistryAuth
	Source string

	reported *atomic.Bool
}

func NewCredential(a config.RegistryAuth, source string) Credential {
	return Credential{Auth: a, Source: source, reported: new(atomic.Bool)}
}

func CentralCredentials(auths []config.RegistryAuth) []Credential {
	out := make([]Credential, 0, len(auths))
	for i, a := range auths {
		out = append(out, NewCredential(a, fmt.Sprintf("registry[%d]", i)))
	}
	return out
}

func (c Credential) MarkReported() bool {
	return c.reported != nil && c.reported.CompareAndSwap(false, true)
}

func (c Credential) Scope() string {
	host, path := c.Auth.Scope()
	host = canonicalHost(host)
	if path == "" {
		return host
	}
	return host + "/" + path
}

func (c Credential) String() string {
	return fmt.Sprintf("%s (%s, user %s)", c.Source, c.Scope(), c.Auth.Username)
}

func (c Credential) identity() string {
	user, pass, err := c.Auth.Credentials()
	if err != nil {
		return c.Scope() + "\x00" + c.Auth.Username + "\x00unreadable:" + c.Source
	}
	return credentialKey(c.Scope(), user, pass)
}

func credentialKey(scope, user, pass string) string {
	if user == "" && pass == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(pass))
	return scope + "\x00" + user + "\x00" + hex.EncodeToString(sum[:16])
}

func Candidates(list []Credential, ref Reference) []Credential {
	host := canonicalHost(ref.Registry)
	repo := strings.ToLower(ref.Repo)
	var out []Credential
	bestLen, bestScope := -1, ""
	for _, c := range list {
		h, path := c.Auth.Scope()
		h = canonicalHost(h)
		if h != host && !strings.HasSuffix(host, "."+h) {
			continue
		}
		if path != "" && repo != path && !strings.HasPrefix(repo, path+"/") {
			continue
		}
		switch scope := h + "/" + path; {
		case len(path) > bestLen:
			out, bestLen, bestScope = []Credential{c}, len(path), scope
		case len(path) == bestLen && scope == bestScope:
			out = append(out, c)
		}
	}
	return out
}

type Keyring struct {
	logger *slog.Logger

	mu     sync.Mutex
	prefer map[string]string
}

func NewKeyring(logger *slog.Logger) *Keyring {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Keyring{logger: logger, prefer: map[string]string{}}
}

func (k *Keyring) Credentials(project string, list []Credential, ref Reference, note string) *Credentials {
	repo := canonicalHost(ref.Registry) + "/" + strings.ToLower(ref.Repo)
	return &Credentials{ring: k, key: project + "\x00" + repo, repo: repo, list: Candidates(list, ref), note: note}
}

func (k *Keyring) preferred(key string) string {
	if k == nil {
		return ""
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.prefer[key]
}

func (k *Keyring) remember(key, id string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.prefer[key] = id
}

type Credentials struct {
	ring *Keyring
	key  string
	repo string
	list []Credential
	note string
}

func (cs *Credentials) Len() int {
	if cs == nil {
		return 0
	}
	return len(cs.list)
}

func (cs *Credentials) Note() string {
	if cs == nil {
		return ""
	}
	return cs.note
}

func (cs *Credentials) First() *config.RegistryAuth {
	if cs.Len() == 0 {
		return nil
	}
	a := cs.ordered()[0].Auth
	return &a
}

func (cs *Credentials) ordered() []Credential {
	pref := cs.ring.preferred(cs.key)
	if pref == "" {
		return cs.list
	}
	out := make([]Credential, 0, len(cs.list))
	var rest []Credential
	for _, c := range cs.list {
		if c.identity() == pref {
			out = append(out, c)
		} else {
			rest = append(rest, c)
		}
	}
	return append(out, rest...)
}

func (cs *Credentials) Try(fn func(*config.RegistryAuth) error, isDenied func(error) bool) error {
	if cs.Len() == 0 {
		err := fn(nil)
		if err != nil && cs.Note() != "" && isDenied(err) {
			return fmt.Errorf("%w (%s)", err, cs.note)
		}
		return err
	}
	var order []Credential
	var ids []string
	seen := map[string]bool{}
	for _, c := range cs.ordered() {
		if id := c.identity(); !seen[id] {
			seen[id] = true
			order, ids = append(order, c), append(ids, id)
		}
	}
	var rejected []string
	var err error
	for i, c := range order {
		a := c.Auth
		if err = fn(&a); err == nil {
			if cs.ring.preferred(cs.key) != ids[i] {
				cs.ring.remember(cs.key, ids[i])
			}
			return nil
		}
		if !isDenied(err) {
			return err
		}
		rejected = append(rejected, c.String())
		cs.reportRejected(c, err, i < len(order)-1)
	}
	msg := "rejected credentials: " + strings.Join(rejected, "; ")
	if cs.note != "" {
		msg += "; " + cs.note
	}
	return fmt.Errorf("%w (%s)", err, msg)
}

func (cs *Credentials) reportRejected(c Credential, err error, more bool) {
	if cs.ring == nil || !c.MarkReported() {
		return
	}
	msg := "registry rejected the credentials"
	if more {
		msg += "; trying the next entry for this registry"
	}
	cs.ring.logger.Warn(msg, "source", c.Source, "scope", c.Scope(), "username", c.Auth.Username, "repository", cs.repo, "err", err)
}

type credentialsKey struct{}

func WithCredentials(ctx context.Context, cs *Credentials) context.Context {
	return context.WithValue(ctx, credentialsKey{}, cs)
}

func credentialsFrom(ctx context.Context) (*Credentials, bool) {
	cs, ok := ctx.Value(credentialsKey{}).(*Credentials)
	return cs, ok
}

func IsDenied(err error) bool { return errors.Is(err, ErrDenied) }
