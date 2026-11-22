package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPIBase    = "https://api.bot.qq.com"
	defaultGatewayURL = "wss://api.bot.qq.com/websocket"
	maxUploadBase64   = 5 << 20
)

// flexInt accepts either a JSON number or a JSON string, because expires_in
// has appeared in both forms in the platform documentation.
type flexInt int

func (f *flexInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(strings.TrimSpace(string(data)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// errCodeMessageDeduplicated is returned when the same msg_id + msg_seq has
// already been answered, for example by a second running bot instance.
const errCodeMessageDeduplicated = 40054005

type apiError struct {
	Code       int
	Message    string
	TraceID    string
	HTTPStatus int
}

func isDuplicateMessageError(err error) bool {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Code == errCodeMessageDeduplicated
	}
	return false
}

func (e *apiError) Error() string {
	if e.TraceID != "" {
		return fmt.Sprintf("QQ API error %d: %s (trace %s)", e.Code, e.Message, e.TraceID)
	}
	return fmt.Sprintf("QQ API error %d: %s", e.Code, e.Message)
}

// qqAPI is a minimal client for the official QQ bot HTTP API. The token is
// cached and refreshed shortly before it expires.
type qqAPI struct {
	appID        string
	clientSecret string
	baseURL      string
	httpClient   *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newQQAPI(appID, clientSecret string) *qqAPI {
	return &qqAPI{
		appID:        appID,
		clientSecret: clientSecret,
		baseURL:      defaultAPIBase,
		httpClient:   &http.Client{Timeout: 20 * time.Second},
	}
}

type tokenResponse struct {
	AccessToken string  `json:"access_token"`
	ExpiresIn   flexInt `json:"expires_in"`
	Code        int     `json:"code"`
	Message     string  `json:"message"`
}

func (a *qqAPI) accessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.token != "" && time.Now().Before(a.expires.Add(-60*time.Second)) {
		return a.token, nil
	}

	body, err := json.Marshal(map[string]string{
		"appId":        a.appID,
		"clientSecret": a.clientSecret,
	})
	if err != nil {
		return "", fmt.Errorf("marshal token request: %w", err)
	}

	data, status, header, err := a.do(ctx, http.MethodPost, "/app/getAppAccessToken", "", body)
	if err != nil {
		return "", err
	}

	var resp tokenResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse token response (HTTP %d): %w", status, err)
	}
	trace := header.Get("X-Tps-trace-ID")
	if resp.Code != 0 {
		return "", &apiError{Code: resp.Code, Message: resp.Message, TraceID: trace, HTTPStatus: status}
	}
	if status/100 != 2 {
		return "", &apiError{Code: status, Message: strings.TrimSpace(string(data)), TraceID: trace, HTTPStatus: status}
	}
	if resp.AccessToken == "" {
		return "", errors.New("token response missing access_token")
	}

	expires := int(resp.ExpiresIn)
	if expires <= 0 {
		expires = 7200
	}
	a.token = resp.AccessToken
	a.expires = time.Now().Add(time.Duration(expires) * time.Second)
	log.Printf("[api] access token refreshed, expires in %ds", expires)
	return a.token, nil
}

func (a *qqAPI) invalidateToken() {
	a.mu.Lock()
	a.token = ""
	a.expires = time.Time{}
	a.mu.Unlock()
}

// request sends an authorized API request. A 401 triggers one token refresh
// and one retry; other errors are returned to the caller.
func (a *qqAPI) request(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		body, err = json.Marshal(in)
		if err != nil {
			return fmt.Errorf("marshal %s request: %w", path, err)
		}
	}

	token, err := a.accessToken(ctx)
	if err != nil {
		return err
	}
	data, status, header, err := a.do(ctx, method, path, token, body)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		a.invalidateToken()
		token, err = a.accessToken(ctx)
		if err != nil {
			return err
		}
		data, status, header, err = a.do(ctx, method, path, token, body)
		if err != nil {
			return err
		}
	}
	return decodeResponse(path, status, header, data, out)
}

func decodeResponse(path string, status int, header http.Header, data []byte, out any) error {
	var env struct {
		ErrCode int    `json:"err_code"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		TraceID string `json:"trace_id"`
	}
	_ = json.Unmarshal(data, &env)

	trace := env.TraceID
	if trace == "" {
		trace = header.Get("X-Tps-trace-ID")
	}
	code := env.ErrCode
	if code == 0 {
		code = env.Code
	}
	if code != 0 {
		return &apiError{Code: code, Message: env.Message, TraceID: trace, HTTPStatus: status}
	}
	if status/100 != 2 {
		return &apiError{Code: status, Message: strings.TrimSpace(string(data)), TraceID: trace, HTTPStatus: status}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("parse %s response: %w", path, err)
		}
	}
	return nil
}

func (a *qqAPI) do(ctx context.Context, method, path, token string, body []byte) ([]byte, int, http.Header, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, reader)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "QQBot "+token)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, resp.Header, fmt.Errorf("read %s response: %w", path, err)
	}
	return data, resp.StatusCode, resp.Header, nil
}

func (a *qqAPI) gatewayURL(ctx context.Context) (string, error) {
	var resp struct {
		URL string `json:"url"`
	}
	if err := a.request(ctx, http.MethodGet, "/gateway/bot", nil, &resp); err != nil {
		log.Printf("[api] gateway lookup failed: %v; using default URL", err)
		return defaultGatewayURL, nil
	}
	if resp.URL == "" {
		return defaultGatewayURL, nil
	}
	return resp.URL, nil
}

func (a *qqAPI) sendText(ctx context.Context, target chatTarget, msgID string, seq int, text string) error {
	payload := map[string]any{
		"msg_type": 0,
		"content":  text,
		"msg_id":   msgID,
		"msg_seq":  seq,
	}
	return a.request(ctx, http.MethodPost, target.path("/messages"), payload, nil)
}

func (a *qqAPI) uploadImage(ctx context.Context, target chatTarget, png []byte) (string, error) {
	if len(png) == 0 {
		return "", errors.New("image is empty")
	}
	encoded := base64.StdEncoding.EncodeToString(png)
	if len(encoded) > maxUploadBase64 {
		return "", fmt.Errorf("image too large: %d bytes", len(png))
	}

	payload := map[string]any{
		"file_type":    1,
		"file_name":    "servers.png",
		"file_data":    encoded,
		"srv_send_msg": false,
	}
	var resp struct {
		FileInfo string `json:"file_info"`
		TTL      int    `json:"ttl"`
	}
	if err := a.request(ctx, http.MethodPost, target.path("/files"), payload, &resp); err != nil {
		return "", err
	}
	if resp.FileInfo == "" {
		return "", errors.New("upload response missing file_info")
	}
	return resp.FileInfo, nil
}

func (a *qqAPI) sendMedia(ctx context.Context, target chatTarget, msgID string, seq int, fileInfo string) error {
	payload := map[string]any{
		"msg_type": 7,
		"media":    map[string]string{"file_info": fileInfo},
		"msg_id":   msgID,
		"msg_seq":  seq,
	}
	return a.request(ctx, http.MethodPost, target.path("/messages"), payload, nil)
}
