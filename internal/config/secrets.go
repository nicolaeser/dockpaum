package config

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
)

const maxSecretFileSize = 64 * 1024

func readSecretFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFileSize+1))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > maxSecretFileSize {
		return "", fmt.Errorf("%s exceeds %d bytes", path, maxSecretFileSize)
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

func secretFromFile(key string, value *string, file string) error {
	file = strings.TrimSpace(file)
	if file == "" {
		return nil
	}
	if *value != "" {
		name := key[strings.LastIndexByte(key, '.')+1:]
		return fmt.Errorf("%s: set either %s or %s_file, not both", key, name, name)
	}
	s, err := readSecretFile(file)
	if err != nil {
		return fmt.Errorf("%s_file: %w", key, err)
	}
	*value = s
	return nil
}

func headersFromFiles(key string, headers map[string]string, files map[string]string) (map[string]string, error) {
	if len(files) == 0 {
		return headers, nil
	}
	out := make(map[string]string, len(headers)+len(files))
	for k, v := range headers {
		out[k] = v
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%s.header_files: empty header name", key)
		}
		for k := range headers {
			if http.CanonicalHeaderKey(k) == http.CanonicalHeaderKey(name) {
				return nil, fmt.Errorf("%s: header %s is set in both headers and header_files", key, name)
			}
		}
		v, err := readSecretFile(strings.TrimSpace(files[name]))
		if err != nil {
			return nil, fmt.Errorf("%s.header_files.%s: %w", key, name, err)
		}
		out[name] = v
	}
	return out, nil
}

func (c *Config) resolveSecretFiles() error {
	if err := secretFromFile("http.token", &c.HTTP.Token, c.HTTP.TokenFile); err != nil {
		return err
	}
	if err := secretFromFile("http.read_token", &c.HTTP.ReadToken, c.HTTP.ReadTokenFile); err != nil {
		return err
	}
	for i := range c.Notify {
		n := &c.Notify[i]
		base := fmt.Sprintf("notify[%d]", i)
		if err := secretFromFile(base+".url", &n.URL, n.URLFile); err != nil {
			return err
		}
		if err := secretFromFile(base+".token", &n.Token, n.TokenFile); err != nil {
			return err
		}
		h, err := headersFromFiles(base, n.Headers, n.HeaderFiles)
		if err != nil {
			return err
		}
		n.Headers = h
		if n.SMTP != nil && n.SMTP.PasswordFile != "" {
			smtp := *n.SMTP
			if err := secretFromFile(base+".smtp.password", &smtp.Password, smtp.PasswordFile); err != nil {
				return err
			}
			n.SMTP = &smtp
		}
	}
	if a := c.Update.Approval; a != nil && len(a.HeaderFiles) > 0 {
		approval := *a
		h, err := headersFromFiles("update.approval", approval.Headers, approval.HeaderFiles)
		if err != nil {
			return err
		}
		approval.Headers = h
		c.Update.Approval = &approval
	}
	return nil
}
