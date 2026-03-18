package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

const (
	// copilotClientID is the GitHub OAuth client ID used by the Copilot CLI integration.
	copilotClientID = "Iv1.b507a08c87ecfe98"
	// copilotScope is the OAuth scope requested during device flow.
	copilotScope = "read:user"

	// GitHub OAuth endpoints.
	githubDeviceCodeURL   = "https://github.com/login/device/code"
	githubAccessTokenURL  = "https://github.com/login/oauth/access_token"
	githubCopilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	githubUserInfoURL     = "https://api.github.com/user"

	// CopilotAPIBaseURL is the base URL for the Copilot OpenAI-compatible API.
	CopilotAPIBaseURL = "https://api.githubcopilot.com"

	defaultPollInterval = 5 * time.Second
	maxPollDuration     = 15 * time.Minute
)

// CopilotAuth handles the GitHub Copilot authentication flow.
type CopilotAuth struct {
	httpClient *http.Client
}

// NewCopilotAuth constructs a new CopilotAuth with proxy-aware transport.
func NewCopilotAuth(cfg *config.Config) *CopilotAuth {
	client := &http.Client{Timeout: 30 * time.Second}
	if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	return &CopilotAuth{httpClient: client}
}

// RequestDeviceCode initiates the GitHub device flow.
func (a *CopilotAuth) RequestDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	form := url.Values{}
	form.Set("client_id", copilotClientID)
	form.Set("scope", copilotScope)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubDeviceCodeURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("copilot: create device code request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot: device code request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("copilot: read device code response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot: device code request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var dc DeviceCodeResponse
	if err = json.Unmarshal(body, &dc); err != nil {
		return nil, fmt.Errorf("copilot: parse device code response failed: %w", err)
	}
	if dc.DeviceCode == "" {
		return nil, fmt.Errorf("copilot: device code is empty in response")
	}
	if dc.VerificationURIComplete == "" {
		dc.VerificationURIComplete = dc.VerificationURI
	}
	return &dc, nil
}

// PollForGithubToken polls the GitHub token endpoint until the user authorizes.
func (a *CopilotAuth) PollForGithubToken(ctx context.Context, dc *DeviceCodeResponse) (string, error) {
	if dc == nil {
		return "", fmt.Errorf("copilot: device code response is nil")
	}

	interval := defaultPollInterval
	if dc.Interval > 0 {
		interval = time.Duration(dc.Interval) * time.Second
	}

	deadline := time.Now().Add(maxPollDuration)
	if dc.ExpiresIn > 0 {
		codeDeadline := time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
		if codeDeadline.Before(deadline) {
			deadline = codeDeadline
		}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("copilot: context cancelled: %w", ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				return "", fmt.Errorf("copilot: device code expired")
			}

			token, pollErr, shouldContinue := a.exchangeDeviceCode(ctx, dc.DeviceCode, &interval)
			if token != "" {
				return token, nil
			}
			if !shouldContinue {
				return "", pollErr
			}
			// Reset ticker in case interval was increased (slow_down).
			ticker.Reset(interval)
		}
	}
}

// exchangeDeviceCode attempts to exchange the device code for a GitHub access token.
// Returns (token, error, shouldContinue).
func (a *CopilotAuth) exchangeDeviceCode(ctx context.Context, deviceCode string, interval *time.Duration) (string, error, bool) {
	form := url.Values{}
	form.Set("client_id", copilotClientID)
	form.Set("device_code", deviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, githubAccessTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("copilot: create token request failed: %w", err), false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot: token request failed: %w", err), false
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("copilot: read token response failed: %w", err), false
	}

	var tokenResp struct {
		AccessToken      string `json:"access_token"`
		TokenType        string `json:"token_type"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("copilot: parse token response failed: %w", err), false
	}

	if tokenResp.Error != "" {
		switch tokenResp.Error {
		case "authorization_pending":
			return "", nil, true
		case "slow_down":
			if interval != nil {
				*interval += 5 * time.Second
			}
			return "", nil, true
		case "expired_token":
			return "", fmt.Errorf("copilot: device code expired"), false
		case "access_denied":
			return "", fmt.Errorf("copilot: access denied by user"), false
		default:
			return "", fmt.Errorf("copilot: OAuth error: %s - %s", tokenResp.Error, tokenResp.ErrorDescription), false
		}
	}

	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("copilot: empty access token in response"), false
	}
	return tokenResp.AccessToken, nil, false
}

// FetchCopilotToken exchanges a GitHub OAuth token for a short-lived Copilot API token.
func (a *CopilotAuth) FetchCopilotToken(ctx context.Context, githubToken string) (*CopilotTokenData, error) {
	if strings.TrimSpace(githubToken) == "" {
		return nil, fmt.Errorf("copilot: github token is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubCopilotTokenURL, nil)
	if err != nil {
		return nil, fmt.Errorf("copilot: create copilot token request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot: copilot token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("copilot: read copilot token response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		log.Debugf("copilot token request failed: status=%d body=%s", resp.StatusCode, string(body))
		return nil, fmt.Errorf("copilot: copilot token request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("copilot: parse copilot token response failed: %w", err)
	}
	if strings.TrimSpace(tokenResp.Token) == "" {
		return nil, fmt.Errorf("copilot: empty copilot token in response")
	}

	return &CopilotTokenData{
		Token:     strings.TrimSpace(tokenResp.Token),
		ExpiresAt: tokenResp.ExpiresAt,
	}, nil
}

// FetchGithubUserLogin retrieves the GitHub username for the given token.
func (a *CopilotAuth) FetchGithubUserLogin(ctx context.Context, githubToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubUserInfoURL, nil)
	if err != nil {
		return "", fmt.Errorf("copilot: create user info request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("copilot: user info request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("copilot: read user info response failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("copilot: user info request failed with status %d", resp.StatusCode)
	}

	var userResp struct {
		Login string `json:"login"`
		Email string `json:"email"`
	}
	if err = json.Unmarshal(body, &userResp); err != nil {
		return "", fmt.Errorf("copilot: parse user info response failed: %w", err)
	}
	return strings.TrimSpace(userResp.Login), nil
}

// CreateTokenStorage converts authentication results into persistence storage.
func (a *CopilotAuth) CreateTokenStorage(githubToken, login string, copilotTokenData *CopilotTokenData) *CopilotTokenStorage {
	storage := &CopilotTokenStorage{
		GithubToken: githubToken,
		Login:       login,
		Email:       login,
		LastRefresh: time.Now().Format(time.RFC3339),
		Type:        "copilot",
	}
	if copilotTokenData != nil {
		storage.CopilotToken = copilotTokenData.Token
		if copilotTokenData.ExpiresAt > 0 {
			storage.CopilotTokenExpiry = time.Unix(copilotTokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
		}
	}
	return storage
}
