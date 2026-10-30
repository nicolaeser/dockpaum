package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type Version struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Date     string `json:"date"`
	Go       string `json:"go,omitempty"`
	Platform string `json:"platform,omitempty"`
}

type ReadinessProbe func() error

type TriggerRequest struct {
	Containers []string
	Policy     string
	Images     []string
	Endpoint   string
	DryRun     bool
}

type API struct {
	Trigger           func(TriggerRequest) string
	Status            func() any
	Containers        func(ContainersQuery) any
	Summary           func() Summary
	Busy              func() Busy
	WatchtowerMetrics http.Handler
	WatchtowerCompat  bool
}

type Summary struct {
	Monitored      int        `json:"monitored"`
	PendingUpdates int        `json:"pending_updates"`
	Gated          int        `json:"gated"`
	Updated24h     int        `json:"updated_24h"`
	Failed24h      int        `json:"failed_24h"`
	DryRun         bool       `json:"dry_run"`
	NextRunAt      *time.Time `json:"next_run_at"`
	Version        string     `json:"version"`
	Instance       string     `json:"instance"`
}

type Busy struct {
	Busy      bool      `json:"busy"`
	RunID     string    `json:"run_id,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
}

type ContainersQuery struct {
	Hosts       []string
	Containers  []string
	PendingOnly bool
}

func (q ContainersQuery) Matches(host, container string, pending bool) bool {
	if q.PendingOnly && !pending {
		return false
	}
	if len(q.Hosts) > 0 && !slices.Contains(q.Hosts, host) {
		return false
	}
	return len(q.Containers) == 0 || slices.Contains(q.Containers, container)
}

const (
	EndpointInternal   = "internal-api"
	EndpointWatchtower = "watchtower"
)

const maxTriggerTargets = 256

type Server struct {
	cfg    config.HTTPConfig
	server *http.Server
	logger *slog.Logger

	ready ReadinessProbe
}

type access int

const (
	accessRead access = iota
	accessWrite
)

func New(cfg config.HTTPConfig, metricsHandler http.Handler, logger *slog.Logger, ver Version, probe ReadinessProbe, api API) *Server {
	s := &Server{cfg: cfg, logger: logger, ready: probe}

	mux := http.NewServeMux()

	healthz := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	readyz := func(w http.ResponseWriter, _ *http.Request) {
		if s.ready != nil {
			if err := s.ready(); err != nil {
				http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	}
	versionH := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ver)
	}

	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	update := func(endpoint string, dryRun bool, methods ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !cfg.UpdateAPI || api.Trigger == nil {
				http.NotFound(w, r)
				return
			}
			if !slices.Contains(methods, r.Method) {
				w.Header().Set("Allow", strings.Join(methods, ", "))
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			req, err := parseTrigger(r.URL.Query())
			if err != nil {
				http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
				return
			}
			req.Endpoint = endpoint
			req.DryRun = dryRun
			status := api.Trigger(req)
			code := http.StatusAccepted
			switch status {
			case "unavailable":
				code = http.StatusServiceUnavailable
			case "too-many-queued":
				code = http.StatusTooManyRequests
			}
			writeJSON(w, code, map[string]string{"status": status})
		}
	}
	status := func(w http.ResponseWriter, r *http.Request) {
		if api.Status == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, api.Status())
	}
	containers := func(w http.ResponseWriter, r *http.Request) {
		if api.Containers == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q, err := parseContainersQuery(r.URL.Query())
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"containers": api.Containers(q)})
	}
	summary := func(w http.ResponseWriter, r *http.Request) {
		if api.Summary == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, http.StatusOK, api.Summary())
	}
	busy := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var b Busy
		if api.Busy != nil {
			b = api.Busy()
		}
		if !b.Busy {
			b = Busy{}
		}
		if !b.StartedAt.IsZero() {
			b.StartedAt = b.StartedAt.UTC().Truncate(time.Second)
		}
		writeJSON(w, http.StatusOK, b)
	}
	watchtowerMetrics := func(w http.ResponseWriter, r *http.Request) {
		if api.WatchtowerMetrics == nil {
			http.NotFound(w, r)
			return
		}
		api.WatchtowerMetrics.ServeHTTP(w, r)
	}

	mux.Handle("/internal-api/health", s.guard(accessRead, http.HandlerFunc(healthz)))
	mux.Handle("/internal-api/ready", s.guard(accessRead, http.HandlerFunc(readyz)))
	mux.Handle("/internal-api/busy", s.guard(accessRead, http.HandlerFunc(busy)))
	mux.Handle("/internal-api/version", s.guard(accessRead, http.HandlerFunc(versionH)))
	mux.Handle("/internal-api/metrics", s.guard(accessRead, metricsHandler))
	mux.Handle("/internal-api/v1/status", s.guard(accessRead, http.HandlerFunc(status)))
	mux.Handle("/internal-api/v1/containers", s.guard(accessRead, http.HandlerFunc(containers)))
	mux.Handle("/internal-api/v1/summary", s.guard(accessRead, http.HandlerFunc(summary)))
	mux.Handle("/internal-api/v1/update", s.guard(accessWrite, update(EndpointInternal, false, http.MethodPost)))
	mux.Handle("/internal-api/v1/check", s.guard(accessWrite, update(EndpointInternal, true, http.MethodPost)))
	if api.WatchtowerCompat {
		mux.Handle("/v1/update", s.guard(accessWrite, update(EndpointWatchtower, false, http.MethodGet, http.MethodPost)))
		mux.Handle("/v1/metrics", s.guard(accessRead, http.HandlerFunc(watchtowerMetrics)))
	}

	s.server = &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *Server) guard(need access, h http.Handler) http.Handler {
	full, read := s.cfg.Token, s.cfg.ReadToken
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if need == accessRead && full == "" && read == "" {
			h.ServeHTTP(w, r)
			return
		}
		if need == accessWrite && full == "" {
			http.NotFound(w, r)
			return
		}
		got, ok := bearer(r)
		isFull := ok && tokenEqual(got, full)
		isRead := ok && tokenEqual(got, read)
		switch {
		case isFull, need == accessRead && isRead:
			h.ServeHTTP(w, r)
		case isRead:
			http.Error(w, "forbidden: http.read_token cannot trigger runs", http.StatusForbidden)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="dockpaum"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	})
}

func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return h[len(prefix):], true
}

func tokenEqual(got, want string) bool {
	if want == "" {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func parseTrigger(q url.Values) (TriggerRequest, error) {
	var req TriggerRequest
	for _, p := range q["policy"] {
		if p = strings.TrimSpace(p); p == "" || p == req.Policy {
			continue
		}
		if req.Policy != "" {
			return TriggerRequest{}, fmt.Errorf("more than one policy: %q and %q", req.Policy, p)
		}
		req.Policy = p
	}
	tooMany := fmt.Errorf("more than %d container and image values", maxTriggerTargets)
	seenContainer := map[string]bool{}
	for _, c := range q["container"] {
		if c = strings.TrimPrefix(strings.TrimSpace(c), "/"); c != "" && !seenContainer[c] {
			seenContainer[c] = true
			if len(req.Containers) == maxTriggerTargets {
				return TriggerRequest{}, tooMany
			}
			req.Containers = append(req.Containers, c)
		}
	}
	seen := map[string]bool{}
	for _, v := range q["image"] {
		for img := range strings.SplitSeq(v, ",") {
			img = strings.TrimSpace(img)
			if img == "" || seen[img] {
				continue
			}
			if len(req.Containers)+len(req.Images) == maxTriggerTargets {
				return TriggerRequest{}, tooMany
			}
			if err := checkImageParam(img); err != nil {
				return TriggerRequest{}, err
			}
			seen[img] = true
			req.Images = append(req.Images, img)
		}
	}
	return req, nil
}

func parseContainersQuery(q url.Values) (ContainersQuery, error) {
	var out ContainersQuery
	names := func(key string) ([]string, error) {
		var vals []string
		for _, v := range q[key] {
			if v = strings.TrimSpace(v); v == "" {
				continue
			}
			if len(vals) == maxTriggerTargets {
				return nil, fmt.Errorf("more than %d %s values", maxTriggerTargets, key)
			}
			vals = append(vals, v)
		}
		return vals, nil
	}
	var err error
	if out.Hosts, err = names("host"); err != nil {
		return ContainersQuery{}, err
	}
	if out.Containers, err = names("container"); err != nil {
		return ContainersQuery{}, err
	}
	for i, c := range out.Containers {
		out.Containers[i] = strings.TrimPrefix(c, "/")
	}
	if v := strings.TrimSpace(q.Get("pending")); v != "" {
		pending, err := strconv.ParseBool(v)
		if err != nil {
			return ContainersQuery{}, fmt.Errorf("pending: %q is not true or false", v)
		}
		out.PendingOnly = pending
	}
	return out, nil
}

func checkImageParam(img string) error {
	if len(img) > 512 {
		return errors.New("image: reference longer than 512 bytes")
	}
	if strings.IndexFunc(img, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return fmt.Errorf("image %q: contains whitespace or control characters", img)
	}
	return nil
}

func (s *Server) Handler() http.Handler { return s.server.Handler }

func (s *Server) Run(_ context.Context) error {
	if !s.cfg.IsEnabled() {
		s.logger.Info("http server disabled")
		return nil
	}
	s.logger.Info("http server listening", "addr", s.cfg.ListenAddr())
	return s.server.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	if !s.cfg.IsEnabled() {
		return nil
	}
	return s.server.Shutdown(ctx)
}
