package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nicolaeser/dockpaum/internal/audit"
	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/notify"
)

const (
	approvalPhaseInitial = "initial"
	approvalPhaseFinal   = "final"
)

const (
	approvalApproved = "approved"
	approvalDenied   = "denied"
	approvalError    = "error"
)

const (
	maxApprovalAnswer = 64 << 10
	maxApprovalReason = 256
	maxApprovalRetry  = 7 * 24 * time.Hour
)

type approvalRequest struct {
	RequestID     string            `json:"request_id"`
	Phase         string            `json:"phase"`
	Host          string            `json:"host"`
	ContainerID   string            `json:"container_id"`
	Container     string            `json:"container"`
	Image         string            `json:"image"`
	OldDigest     string            `json:"old_digest"`
	NewDigest     string            `json:"new_digest"`
	OldRepoDigest string            `json:"old_repo_digest,omitempty"`
	NewRepoDigest string            `json:"new_repo_digest,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	At            string            `json:"at"`
}

type approvalSubject struct {
	OldDigest, NewDigest         string
	OldRepoDigest, NewRepoDigest string
	Labels                       map[string]string
}

type approvalAnswer struct {
	approved bool
	reason   string
	result   string
	status   *ApprovalStatus
}

type approvalGate struct {
	cfg    *config.ApprovalConfig
	client *http.Client
	signer *approvalSigner
	now    func() time.Time
}

func newApprovalGate(cfg *config.ApprovalConfig, logger *slog.Logger, now func() time.Time) *approvalGate {
	if cfg == nil || cfg.URL == "" {
		return nil
	}
	g := &approvalGate{cfg: cfg, client: approvalClient, now: now}
	if cfg.Signing != nil {
		g.signer = newApprovalSigner(*cfg.Signing, logger)
		g.client = signedApprovalClient
	}
	return g
}

func (g *approvalGate) recheck() bool {
	return g != nil && g.cfg.RecheckBeforeStop
}

func (g *approvalGate) ask(ctx context.Context, host string, fc *docker.FullContainer, subject approvalSubject, phase string) (approvalAnswer, error) {
	if g == nil {
		return approvalAnswer{approved: true, result: approvalApproved}, nil
	}
	cfg := g.cfg
	method := strings.ToUpper(strings.TrimSpace(cfg.Method))
	if method == "" {
		method = http.MethodPost
	}
	timeout := cfg.Timeout.Std()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	asked := g.now()

	body := approvalRequest{
		RequestID:     approvalRequestID(host, fc.Name, subject.NewDigest),
		Phase:         phase,
		Host:          host,
		ContainerID:   fc.ID,
		Container:     fc.Name,
		Image:         fc.Image,
		OldDigest:     subject.OldDigest,
		NewDigest:     subject.NewDigest,
		OldRepoDigest: subject.OldRepoDigest,
		NewRepoDigest: subject.NewRepoDigest,
		At:            asked.UTC().Format(time.RFC3339),
	}
	labels := subject.Labels
	if labels == nil && fc.Config != nil {
		labels = fc.Config.Labels
	}
	body.Labels = filterLabels(labels, cfg.LabelAllowlist)
	buf, err := json.Marshal(body)
	if err != nil {
		return approvalAnswer{}, err
	}

	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	requestURL := cfg.URL
	var sent []byte
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodDelete {
		withQuery, err := appendApprovalQuery(cfg.URL, body)
		if err != nil {
			return approvalAnswer{}, err
		}
		requestURL = withQuery
	} else {
		sent = buf
	}
	var reqBody io.Reader
	if sent != nil {
		reqBody = bytes.NewReader(sent)
	}
	req, err := http.NewRequestWithContext(rctx, method, requestURL, reqBody)
	if err != nil {
		return approvalAnswer{}, err
	}
	if sent != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "dockpaum/approval")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	status := &ApprovalStatus{RequestID: body.RequestID, Phase: phase, AskedAt: asked.UTC()}
	ans := approvalAnswer{status: status}
	origin := approvalOrigin(cfg.URL)
	which := ""
	if phase == approvalPhaseFinal {
		which = " at the final check before the stop"
	}

	if g.signer != nil {
		if err := g.signer.sign(req, sent, asked); err != nil {
			ans.result = approvalError
			ans.reason = fmt.Sprintf("approval request to %s%s not sent: %v", origin, which, err)
			return ans, nil
		}
	}

	resp, err := g.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		ans.result = approvalError
		if cfg.ApprovesOnFailure() {
			ans.approved, status.Approved = true, true
			ans.reason = fmt.Sprintf("approved: %s unreachable%s (approve_on: failure): %v", origin, which, err)
			return ans, nil
		}
		if cfg.FailOpen {
			ans.approved, status.Approved = true, true
			ans.reason = fmt.Sprintf("approval transport error (fail-open) from %s%s: %v", origin, which, err)
		} else {
			ans.reason = fmt.Sprintf("approval transport error from %s%s: %v", origin, which, err)
		}
		return ans, nil
	}
	defer resp.Body.Close()
	answer := readApprovalAnswer(resp.Body)

	status.HTTPStatus = resp.StatusCode
	status.Reason = answer.reason
	if cfg.ApprovesOnFailure() {
		ans.approved = !statusOK(resp.StatusCode, cfg.OkStatuses)
		status.Approved = ans.approved
		if ans.approved {
			ans.result = approvalApproved
			ans.reason = fmt.Sprintf("approved: %s answered HTTP %d%s (approve_on: failure)", origin, resp.StatusCode, which)
		} else {
			ans.result = approvalDenied
			ans.reason = fmt.Sprintf("denied: %s answered HTTP %d%s (approve_on: failure)", origin, resp.StatusCode, which)
		}
		if answer.reason != "" {
			ans.reason += ": " + answer.reason
		}
		return ans, nil
	}
	explicitNo := answer.approved != nil && !*answer.approved
	ans.approved = statusOK(resp.StatusCode, cfg.OkStatuses) && !explicitNo
	status.Approved = ans.approved

	what := fmt.Sprintf("HTTP %d", resp.StatusCode)
	if explicitNo {
		what += ", approved: false"
	}
	if ans.approved {
		ans.result = approvalApproved
		ans.reason = fmt.Sprintf("approved by %s%s (%s)", origin, which, what)
	} else {
		ans.result = approvalDenied
		ans.reason = fmt.Sprintf("denied by %s%s (%s)", origin, which, what)
		if wait := retryAfter(resp.Header.Get("Retry-After"), answer.retryAfter, asked); wait > 0 {
			status.RetryAt = asked.Add(wait).UTC()
		}
	}
	if answer.reason != "" {
		ans.reason += ": " + answer.reason
	}
	return ans, nil
}

type approverAnswer struct {
	approved   *bool
	reason     string
	retryAfter time.Duration
}

func readApprovalAnswer(r io.Reader) approverAnswer {
	var out approverAnswer
	b, err := io.ReadAll(io.LimitReader(r, maxApprovalAnswer+1))
	if err != nil || len(b) > maxApprovalAnswer {
		return out
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimSpace(b), &fields) != nil {
		return out
	}
	if raw, ok := fields["approved"]; ok && string(raw) != "null" {
		var v bool
		_ = json.Unmarshal(raw, &v)
		out.approved = &v
	}
	if raw, ok := fields["reason"]; ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			out.reason = cleanApprovalReason(v)
		}
	}
	if raw, ok := fields["retry_after_seconds"]; ok {
		var v float64
		if json.Unmarshal(raw, &v) == nil && v > 0 && !math.IsInf(v, 0) {
			out.retryAfter = time.Duration(math.Min(v, maxApprovalRetry.Seconds())) * time.Second
		}
	}
	return out
}

func retryAfter(header string, fromBody time.Duration, now time.Time) time.Duration {
	wait := fromBody
	header = strings.TrimSpace(header)
	if secs, err := strconv.ParseInt(header, 10, 64); err == nil {
		if secs > 0 {
			wait = max(wait, time.Duration(min(secs, int64(maxApprovalRetry/time.Second)))*time.Second)
		}
	} else if t, err := http.ParseTime(header); err == nil {
		wait = max(wait, t.Sub(now))
	}
	return min(max(wait, 0), maxApprovalRetry)
}

func cleanApprovalReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxApprovalReason {
		return s
	}
	cut := maxApprovalReason - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

var approvalClient = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("too many redirects")
	}
	if first := via[0].URL; req.URL.Scheme != first.Scheme || !strings.EqualFold(req.URL.Host, first.Host) {
		return errors.New("refusing redirect to another origin or scheme")
	}
	return nil
}}

var signedApprovalClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

func approvalOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "the approval endpoint"
	}
	return u.Scheme + "://" + u.Host
}

func appendApprovalQuery(rawURL string, body approvalRequest) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("approval url: %w", err)
	}
	q := u.Query()
	q.Set("request_id", body.RequestID)
	q.Set("phase", body.Phase)
	q.Set("host", body.Host)
	q.Set("container", body.Container)
	q.Set("container_id", body.ContainerID)
	q.Set("image", body.Image)
	q.Set("old_digest", body.OldDigest)
	q.Set("new_digest", body.NewDigest)
	if body.OldRepoDigest != "" {
		q.Set("old_repo_digest", body.OldRepoDigest)
	}
	if body.NewRepoDigest != "" {
		q.Set("new_repo_digest", body.NewRepoDigest)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func approvalRequestID(host, container, newImageID string) string {
	sum := sha256.Sum256([]byte(host + "\x00" + container + "\x00" + newImageID))
	return hex.EncodeToString(sum[:16])
}

func statusOK(code int, allow []int) bool {
	if len(allow) > 0 {
		for _, c := range allow {
			if c == code {
				return true
			}
		}
		return false
	}
	return code >= 200 && code < 300
}

func filterLabels(in map[string]string, allowlist []string) map[string]string {
	if in == nil {
		return nil
	}
	for _, k := range allowlist {
		if k == "*" {
			return in
		}
	}
	out := map[string]string{}
	allowExact := make(map[string]struct{}, len(allowlist))
	for _, k := range allowlist {
		allowExact[k] = struct{}{}
	}
	for k, v := range in {
		if _, ok := allowExact[k]; ok {
			out[k] = v
			continue
		}
		if strings.HasPrefix(k, "dockpaum.") || strings.HasPrefix(k, "com.docker.compose.") {
			out[k] = v
		}
	}
	return out
}

func (u *Updater) approve(ctx context.Context, runID string, c *docker.Client, ct docker.Container, full *docker.FullContainer,
	subject approvalSubject, phase, imageRef string, res *stepResult) (bool, error) {
	ans, err := u.approval.ask(ctx, c.Name, full, subject, phase)
	if err != nil {
		return false, fmt.Errorf("approval: %w", err)
	}
	if ans.status == nil {
		return true, nil
	}
	res.approval = ans.status
	u.deps.Metrics.ApprovalRequests.WithLabelValues(c.Name, phase, ans.result).Inc()
	a := ans.status
	attrs := []any{"host", c.Name, "run_id", runID, "container", ct.Name, "phase", phase,
		"request_id", a.RequestID, "approved", ans.approved, "reason", ans.reason}
	if !a.RetryAt.IsZero() {
		attrs = append(attrs, "retry_at", a.RetryAt.Format(time.RFC3339))
	}
	switch ans.result {
	case approvalError:
		u.deps.Logger.Warn("approval request got no answer", attrs...)
	case approvalDenied:
		u.deps.Logger.Info("update not approved", attrs...)
	default:
		u.deps.Logger.Debug("update approved", attrs...)
	}
	if ans.approved {
		return true, nil
	}
	res.decision = audit.DecisionApprovalDeny
	res.reason = ans.reason
	u.notify(ctx, runID, notify.Event{
		Type: notify.EventDenied, Host: c.Name,
		Container: ct.Name, Image: imageRef,
		OldDigest: res.oldDigest, NewDigest: res.newDigest,
		OldTag: res.oldTag, NewTag: res.newTag,
		Policy: res.policyMode, Reason: ans.reason, ReleaseURL: res.releaseURL,
	})
	return false, nil
}

func (u *Updater) approvalHold(host, container string) (*ApprovalStatus, string) {
	if u.approval == nil {
		return nil, ""
	}
	prev, ok := u.ContainerStatusOf(host, container)
	if !ok || prev.Approval == nil || prev.Approval.Approved || !u.now().Before(prev.Approval.RetryAt) {
		return nil, ""
	}
	a := prev.Approval
	last := fmt.Sprintf("HTTP %d", a.HTTPStatus)
	if a.Reason != "" {
		last += ", " + a.Reason
	}
	return a, fmt.Sprintf("not asked again before %s, as the approval endpoint requested (last answer: %s)",
		a.RetryAt.UTC().Format(time.RFC3339), last)
}
