// Package copilot provides authentication and token management for GitHub Copilot API.
// It handles the OAuth2 Device Authorization Grant flow for secure authentication and
// exchanges GitHub OAuth tokens for short-lived Copilot API tokens.
package copilot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
)

// CopilotTokenStorage persists GitHub Copilot OAuth credentials.
type CopilotTokenStorage struct {
	// GithubToken is the long-lived GitHub OAuth access token.
	GithubToken string `json:"github_token"`
	// CopilotToken is the short-lived Copilot API token derived from GithubToken.
	CopilotToken string `json:"copilot_token,omitempty"`
	// CopilotTokenExpiry is the RFC3339 timestamp when CopilotToken expires.
	CopilotTokenExpiry string `json:"copilot_token_expiry,omitempty"`
	// LastRefresh records the last token refresh timestamp.
	LastRefresh string `json:"last_refresh"`
	// Email is the GitHub account email or login (used for labelling).
	Email string `json:"email,omitempty"`
	// Login is the GitHub account login handle.
	Login string `json:"login,omitempty"`
	// Type indicates the authentication provider type, always "copilot".
	Type string `json:"type"`

	// Metadata holds arbitrary key-value pairs injected via hooks.
	Metadata map[string]any `json:"-"`
}

// SetMetadata allows external callers to inject metadata into the storage before saving.
func (ts *CopilotTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile serialises the token storage to disk.
func (ts *CopilotTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "copilot"
	if err := os.MkdirAll(filepath.Dir(authFilePath), 0o700); err != nil {
		return fmt.Errorf("copilot token: create directory failed: %w", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("copilot token: create file failed: %w", err)
	}
	defer func() { _ = f.Close() }()

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("copilot token: merge metadata failed: %w", errMerge)
	}

	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err = enc.Encode(data); err != nil {
		return fmt.Errorf("copilot token: encode token failed: %w", err)
	}
	return nil
}

// CopilotTokenData holds the raw Copilot token response.
type CopilotTokenData struct {
	// Token is the short-lived Copilot API bearer token.
	Token string
	// ExpiresAt is the Unix timestamp when the Copilot token expires.
	ExpiresAt int64
}

// IsExpired reports whether the Copilot token has expired (or will within 30 s).
func (d *CopilotTokenData) IsExpired() bool {
	if d.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(30 * time.Second).After(time.Unix(d.ExpiresAt, 0))
}

// DeviceCodeResponse models GitHub's device code response.
type DeviceCodeResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}
