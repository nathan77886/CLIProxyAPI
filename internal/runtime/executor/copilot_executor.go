package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	copilotauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const (
	copilotDefaultEndpoint = "/chat/completions"
	copilotUserAgent       = "GitHubCopilotChat/0.26.7"
)

// CopilotExecutor executes OpenAI-compatible chat completions against the GitHub Copilot API.
type CopilotExecutor struct {
	cfg *config.Config
}

// NewCopilotExecutor constructs a new executor instance.
func NewCopilotExecutor(cfg *config.Config) *CopilotExecutor { return &CopilotExecutor{cfg: cfg} }

// Identifier returns the provider key.
func (e *CopilotExecutor) Identifier() string { return "copilot" }

// PrepareRequest injects Copilot credentials into the outgoing HTTP request.
func (e *CopilotExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token := copilotToken(auth)
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// HttpRequest injects Copilot credentials into the request and executes it.
func (e *CopilotExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("copilot executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Execute performs a non-streaming chat completion request.
func (e *CopilotExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token, baseURL := copilotCreds(auth)
	if strings.TrimSpace(token) == "" {
		err = fmt.Errorf("copilot executor: missing api token")
		return resp, err
	}
	if baseURL == "" {
		baseURL = copilotauth.CopilotAPIBaseURL
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, false)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "copilot", e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

	endpoint := strings.TrimSuffix(baseURL, "/") + copilotDefaultEndpoint

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	applyCopilotHeaders(httpReq, token, false)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       endpoint,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("copilot executor: close response body error: %v", errClose)
		}
	}()
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		logWithRequestID(ctx).Debugf("request error, error status: %d error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	reporter.publish(ctx, parseOpenAIUsage(data))
	reporter.ensurePublished(ctx)

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, opts.OriginalRequest, body, data, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream performs a streaming chat completion request.
func (e *CopilotExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token, baseURL := copilotCreds(auth)
	if strings.TrimSpace(token) == "" {
		err = fmt.Errorf("copilot executor: missing api token")
		return nil, err
	}
	if baseURL == "" {
		baseURL = copilotauth.CopilotAPIBaseURL
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayload, true)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, true)
	body, _ = sjson.SetBytes(body, "model", baseModel)

	body, err = thinking.ApplyThinking(body, req.Model, from.String(), "copilot", e.Identifier())
	if err != nil {
		return nil, err
	}

	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

	endpoint := strings.TrimSuffix(baseURL, "/") + copilotDefaultEndpoint

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyCopilotHeaders(httpReq, token, true)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       endpoint,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("copilot executor: close response body error: %v", errClose)
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		logWithRequestID(ctx).Debugf("request error, error status: %d error message: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = statusErr{code: httpResp.StatusCode, msg: string(data)}
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("copilot executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)
			if detail, ok := parseOpenAIStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, opts.OriginalRequest, body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
		reporter.ensurePublished(ctx)
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens estimates token count for Copilot requests.
func (e *CopilotExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, false)

	enc, err := tokenizerForModel(baseModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("copilot executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("copilot executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

// Refresh refreshes the Copilot API token by exchanging the stored GitHub token.
func (e *CopilotExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("copilot executor: refresh called")
	if auth == nil {
		return nil, fmt.Errorf("copilot executor: auth is nil")
	}

	githubToken := ""
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["github_token"].(string); ok {
			githubToken = strings.TrimSpace(v)
		}
	}
	if githubToken == "" {
		log.Debugf("copilot executor: no github_token found in metadata, skipping refresh")
		return auth, nil
	}

	// Check whether existing copilot_token is still valid.
	if !copilotTokenNeedsRefresh(auth.Metadata) {
		log.Debugf("copilot executor: copilot token still valid, skipping refresh")
		return auth, nil
	}

	log.Debugf("copilot executor: refreshing copilot token")
	svc := copilotauth.NewCopilotAuth(e.cfg)
	tokenData, err := svc.FetchCopilotToken(ctx, githubToken)
	if err != nil {
		log.Errorf("copilot executor: token refresh failed: %v", err)
		return nil, err
	}

	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["copilot_token"] = tokenData.Token
	if tokenData.ExpiresAt > 0 {
		auth.Metadata["copilot_token_expiry"] = time.Unix(tokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
		auth.Metadata["expires_at"] = tokenData.ExpiresAt
	}
	auth.Metadata["last_refresh"] = time.Now().Format(time.RFC3339)

	log.Debugf("copilot executor: token refresh successful")
	return auth, nil
}

// copilotTokenNeedsRefresh reports whether the cached Copilot token needs refreshing.
func copilotTokenNeedsRefresh(metadata map[string]any) bool {
	const skew = 60 * time.Second
	if metadata == nil {
		return true
	}
	if v, ok := metadata["copilot_token"].(string); !ok || strings.TrimSpace(v) == "" {
		return true
	}
	if expStr, ok := metadata["copilot_token_expiry"].(string); ok {
		if ts, err := time.Parse(time.RFC3339, strings.TrimSpace(expStr)); err == nil {
			return !ts.After(time.Now().Add(skew))
		}
	}
	if expiresAt, ok := metadata["expires_at"].(float64); ok && expiresAt > 0 {
		return !time.Unix(int64(expiresAt), 0).After(time.Now().Add(skew))
	}
	return true
}

func applyCopilotHeaders(r *http.Request, token string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("User-Agent", copilotUserAgent)
	r.Header.Set("Editor-Version", "vscode/1.95.3")
	r.Header.Set("Editor-Plugin-Version", "copilot-chat/0.22.4")
	r.Header.Set("Openai-Intent", "conversation-panel")
	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
}

// copilotCreds extracts the active Copilot bearer token and optional base URL from auth.
// Preference order: metadata.copilot_token → metadata.access_token → attributes.api_key.
func copilotCreds(a *cliproxyauth.Auth) (token, baseURL string) {
	if a == nil {
		return "", ""
	}
	if a.Metadata != nil {
		if v, ok := a.Metadata["copilot_token"].(string); ok && strings.TrimSpace(v) != "" {
			token = strings.TrimSpace(v)
		}
		if token == "" {
			if v, ok := a.Metadata["access_token"].(string); ok && strings.TrimSpace(v) != "" {
				token = strings.TrimSpace(v)
			}
		}
		if v, ok := a.Metadata["base_url"].(string); ok && strings.TrimSpace(v) != "" {
			baseURL = strings.TrimSpace(v)
		}
	}
	if token == "" && a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["api_key"]); v != "" {
			token = v
		}
	}
	if baseURL == "" && a.Attributes != nil {
		if v := strings.TrimSpace(a.Attributes["base_url"]); v != "" {
			baseURL = v
		}
	}
	return token, baseURL
}

// copilotToken returns only the token portion.
func copilotToken(a *cliproxyauth.Auth) string {
	token, _ := copilotCreds(a)
	return token
}
