package webclient

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/leileipei/Enterprise_IM/internal/httpserver"
)

//go:embed assets/*
var assets embed.FS

var verifierPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

type handler struct {
	base   http.Handler
	auth   httpserver.Authenticator
	config Config
	client *http.Client
	origin string
}

func NewHandler(base http.Handler, auth httpserver.Authenticator, config Config, client *http.Client) (http.Handler, error) {
	if base == nil || auth == nil {
		return nil, errors.New("web handler requires base and authentication")
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("token endpoint redirect rejected")
	}
	redirect, _ := url.Parse(config.RedirectURL)
	originHost := redirect.Host
	if redirect.Port() == "443" {
		originHost = redirect.Hostname()
		if strings.Contains(originHost, ":") {
			originHost = "[" + originHost + "]"
		}
	}
	return &handler{base: base, auth: auth, config: config, client: &copy,
		origin: redirect.Scheme + "://" + originHost}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/web" && !strings.HasPrefix(r.URL.Path, "/web/") {
		h.base.ServeHTTP(w, r)
		return
	}
	h.secureHeaders(w)
	if r.URL.Path == "/web" && r.Method == http.MethodGet {
		http.Redirect(w, r, "/web/", http.StatusPermanentRedirect)
		return
	}
	if r.URL.Path == "/web/oauth/token" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			webError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		h.exchangeToken(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		webError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.URL.Path == "/web/config" {
		if r.URL.RawQuery != "" {
			webError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Issuer           string `json:"issuer"`
			AuthorizationURL string `json:"authorization_url"`
			ClientID         string `json:"client_id"`
			RedirectURL      string `json:"redirect_url"`
			Scope            string `json:"scope"`
		}{h.config.Issuer, h.config.AuthorizationURL, h.config.ClientID, h.config.RedirectURL, h.config.Scope})
		return
	}
	var filename, contentType string
	switch r.URL.Path {
	case "/web/":
		filename, contentType = "assets/index.html", "text/html; charset=utf-8"
	case "/web/file-transport.js":
		filename, contentType = "assets/file-transport.js", "text/javascript; charset=utf-8"
	case "/web/file-transfer.js":
		filename, contentType = "assets/file-transfer.js", "text/javascript; charset=utf-8"
	case "/web/app.js":
		filename, contentType = "assets/app.js", "text/javascript; charset=utf-8"
	case "/web/legal-holds.js":
		filename, contentType = "assets/legal-holds.js", "text/javascript; charset=utf-8"
	case "/web/cross-message-search.js":
		filename, contentType = "assets/cross-message-search.js", "text/javascript; charset=utf-8"
	case "/web/message-search.js":
		filename, contentType = "assets/message-search.js", "text/javascript; charset=utf-8"
	case "/web/audit.js":
		filename, contentType = "assets/audit.js", "text/javascript; charset=utf-8"
	case "/web/retention-history.js":
		filename, contentType = "assets/retention-history.js", "text/javascript; charset=utf-8"
	case "/web/retention-policy.js":
		filename, contentType = "assets/retention-policy.js", "text/javascript; charset=utf-8"
	case "/web/retention.js":
		filename, contentType = "assets/retention.js", "text/javascript; charset=utf-8"
	case "/web/style.css":
		filename, contentType = "assets/style.css", "text/css; charset=utf-8"
	default:
		webError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.URL.RawQuery != "" && (r.URL.Path != "/web/" || !validWebCallbackQuery(r.URL.RawQuery)) {
		webError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	data, err := assets.ReadFile(filename)
	if err != nil {
		webError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func validWebCallbackQuery(raw string) bool {
	if len(raw) > 4096 {
		return false
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return false
	}
	for name, items := range values {
		switch name {
		case "code", "state", "iss", "error", "error_description", "error_uri":
		default:
			return false
		}
		if len(items) != 1 || items[0] == "" || len(items[0]) > 2048 {
			return false
		}
	}
	if _, hasCode := values["code"]; hasCode {
		if _, hasError := values["error"]; hasError || len(values["state"]) != 1 {
			return false
		}
		return len(values) == 2 || (len(values) == 3 && len(values["iss"]) == 1)
	}
	if len(values["error"]) != 1 {
		return false
	}
	return true
}

func (h *handler) secureHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self' wss://"+
		strings.TrimPrefix(h.origin, "https://")+"; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

func (h *handler) exchangeToken(w http.ResponseWriter, r *http.Request) {
	if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != h.origin {
		webError(w, http.StatusForbidden, "invalid_origin")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		webError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" ||
		(len(params) > 0 && (len(params) != 1 || !strings.EqualFold(params["charset"], "utf-8"))) {
		webError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		webError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	code, verifier, err := parseExchangeRequest(raw)
	if err != nil {
		webError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {h.config.ClientID},
		"redirect_uri": {h.config.RedirectURL}, "code": {code}, "code_verifier": {verifier}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, h.config.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		webError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := h.client.Do(request)
	if err != nil {
		webError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		webError(w, http.StatusBadRequest, "exchange_failed")
		return
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		webError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16*1024+1))
	if err != nil || len(body) > 16*1024 {
		webError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	token, err := parseTokenResponse(body)
	if err != nil || !strings.EqualFold(token.TokenType, "Bearer") ||
		len(token.AccessToken) == 0 || len(token.AccessToken) > 8192 || token.ExpiresIn < 1 || token.ExpiresIn > 604800 {
		webError(w, http.StatusServiceUnavailable, "unavailable")
		return
	}
	if _, err := h.auth.Authenticate(ctx, token.AccessToken); err != nil {
		if errors.Is(err, httpserver.ErrAuthUnavailable) {
			webError(w, http.StatusServiceUnavailable, "unavailable")
		} else {
			webError(w, http.StatusUnauthorized, "invalid_identity")
		}
		return
	}
	writeJSON(w, http.StatusOK, struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}{token.AccessToken, "Bearer", token.ExpiresIn})
}

type tokenResponse struct {
	AccessToken string
	TokenType   string
	ExpiresIn   int
}

func parseTokenResponse(body []byte) (tokenResponse, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return tokenResponse{}, errors.New("expected token object")
	}
	seen := map[string]bool{}
	var response tokenResponse
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return tokenResponse{}, errors.New("duplicate or invalid token field")
		}
		seen[name] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return tokenResponse{}, err
		}
		switch name {
		case "access_token":
			err = json.Unmarshal(raw, &response.AccessToken)
		case "token_type":
			err = json.Unmarshal(raw, &response.TokenType)
		case "expires_in":
			err = json.Unmarshal(raw, &response.ExpiresIn)
		}
		if err != nil {
			return tokenResponse{}, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return tokenResponse{}, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return tokenResponse{}, errors.New("trailing token data")
	}
	return response, nil
}

func parseExchangeRequest(raw []byte) (string, string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return "", "", errors.New("expected object")
	}
	values := map[string]string{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok || (name != "code" && name != "code_verifier") {
			return "", "", errors.New("unexpected field")
		}
		if _, duplicate := values[name]; duplicate {
			return "", "", errors.New("duplicate field")
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return "", "", err
		}
		values[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return "", "", err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", "", errors.New("trailing data")
	}
	code, verifier := values["code"], values["code_verifier"]
	if len(code) < 1 || len(code) > 2048 || !verifierPattern.MatchString(verifier) {
		return "", "", errors.New("invalid code or verifier")
	}
	for i := 0; i < len(code); i++ {
		if code[i] < 0x21 || code[i] > 0x7e {
			return "", "", errors.New("invalid code")
		}
	}
	return code, verifier, nil
}

func webError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		ErrorCode string `json:"error_code"`
	}{code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
