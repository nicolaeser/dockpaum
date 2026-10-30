package updater

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nicolaeser/dockpaum/internal/config"
)

const approvalScheme = "DOCKPAUM-APPROVAL-V1"

const (
	headerApprovalKeyID     = "X-Dockpaum-Key-Id"
	headerApprovalTimestamp = "X-Dockpaum-Timestamp"
	headerApprovalNonce     = "X-Dockpaum-Nonce"
	headerApprovalSignature = "X-Dockpaum-Signature"
)

type approvalSigner struct {
	cfg    config.ApprovalSigning
	logger *slog.Logger
	nonce  func() string

	mu     sync.Mutex
	secret []byte
	stamp  fileStamp
}

type fileStamp struct {
	mod  time.Time
	size int64
}

func (a fileStamp) same(b fileStamp) bool { return a.mod.Equal(b.mod) && a.size == b.size }

func newApprovalSigner(cfg config.ApprovalSigning, logger *slog.Logger) *approvalSigner {
	return &approvalSigner{cfg: cfg, logger: logger, nonce: rand.Text}
}

func (s *approvalSigner) key() ([]byte, error) {
	fi, err := os.Stat(s.cfg.SecretFile)
	if err != nil {
		return nil, fmt.Errorf("update.approval.signing.secret_file: %w", err)
	}
	stamp := fileStamp{mod: fi.ModTime(), size: fi.Size()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret != nil && stamp.same(s.stamp) {
		return s.secret, nil
	}
	secret, err := s.cfg.Secret()
	if err != nil {
		s.secret = nil
		return nil, err
	}
	if s.secret != nil {
		s.logger.Info("approval signing secret changed; using the new one", "key_id", s.cfg.KeyID)
	}
	s.secret, s.stamp = secret, stamp
	return secret, nil
}

func (s *approvalSigner) sign(req *http.Request, body []byte, at time.Time) error {
	secret, err := s.key()
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(at.Unix(), 10)
	nonce := s.nonce()
	canonical := approvalCanonical(req.Method, req.URL.RequestURI(), canonicalApprovalHost(req.URL.Scheme, req.Host), ts, nonce, body)
	req.Header.Set(headerApprovalKeyID, s.cfg.KeyID)
	req.Header.Set(headerApprovalTimestamp, ts)
	req.Header.Set(headerApprovalNonce, nonce)
	req.Header.Set(headerApprovalSignature, "v1="+approvalSignature(secret, canonical))
	return nil
}

func approvalCanonical(method, requestURI, host, timestamp, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		approvalScheme, strings.ToUpper(method), requestURI, host, timestamp, nonce, hex.EncodeToString(sum[:]),
	}, "\n")
}

func approvalSignature(secret []byte, canonical string) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(canonical))
	return hex.EncodeToString(m.Sum(nil))
}

func canonicalApprovalHost(scheme, host string) string {
	host = strings.ToLower(host)
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		if strings.Contains(name, ":") {
			return "[" + name + "]"
		}
		return name
	}
	return host
}
