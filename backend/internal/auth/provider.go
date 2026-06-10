package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const twoFactorMessage = "Введите проверочный код 2FA"

// Provider — источник проверки логина/пароля. Реализации: HTTPProvider (внешний GML)
// и LocalProvider (локальная БД с bcrypt+TOTP, см. local_provider.go).
type Provider interface {
	SignIn(ctx context.Context, login, password, totp string) (ProviderSignInResponse, error)
}

type HTTPProvider struct {
	url    string
	client *http.Client
}

func NewHTTPProvider(url string) HTTPProvider {
	return HTTPProvider{
		url: url,
		client: &http.Client{
			Timeout: 12 * time.Second,
		},
	}
}

type ProviderSignInRequest struct {
	Login    string `json:"Login"`
	Password string `json:"Password"`
	Totp     string `json:"Totp,omitempty"`
}

type ProviderSignInResponse struct {
	Login    string `json:"Login"`
	UserUUID string `json:"UserUuid"`
	IsSlim   bool   `json:"IsSlim"`
	Message  string `json:"Message"`
}

type ProviderError struct {
	StatusCode        int
	Message           string
	RequiresTwoFactor bool
}

func (e ProviderError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return fmt.Sprintf("auth provider returned status %d", e.StatusCode)
}

func (p HTTPProvider) SignIn(ctx context.Context, login, password, totp string) (ProviderSignInResponse, error) {
	payload, err := json.Marshal(ProviderSignInRequest{
		Login:    login,
		Password: password,
		Totp:     totp,
	})
	if err != nil {
		return ProviderSignInResponse{}, err
	}

	slog.Info("auth provider request",
		"url", p.url,
		"login", login,
		"has_totp", totp != "",
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(payload))
	if err != nil {
		return ProviderSignInResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := p.client.Do(req)
	if err != nil {
		slog.Error("auth provider network error", "error", err)
		return ProviderSignInResponse{}, err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, 64*1024))
	if err != nil {
		return ProviderSignInResponse{}, err
	}

	slog.Info("auth provider response", "status", res.StatusCode)

	if res.StatusCode == http.StatusOK {
		response := ProviderSignInResponse{Login: login, Message: "Успешная авторизация"}
		if len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, &response); err != nil {
				return ProviderSignInResponse{}, err
			}
		}
		if response.Login == "" {
			response.Login = login
		}
		return response, nil
	}

	message := extractProviderMessage(body)
	if message == "" {
		message = http.StatusText(res.StatusCode)
	}

	return ProviderSignInResponse{}, ProviderError{
		StatusCode:        res.StatusCode,
		Message:           message,
		RequiresTwoFactor: res.StatusCode == http.StatusUnauthorized && (strings.EqualFold(message, twoFactorMessage) || totp != ""),
	}
}

func AsProviderError(err error) (ProviderError, bool) {
	var providerErr ProviderError
	if errors.As(err, &providerErr) {
		return providerErr, true
	}
	return ProviderError{}, false
}

func extractProviderMessage(body []byte) string {
	var payload struct {
		Message string `json:"Message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Message != "" {
		return payload.Message
	}
	return strings.TrimSpace(string(body))
}
