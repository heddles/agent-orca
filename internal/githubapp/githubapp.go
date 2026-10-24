/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package githubapp mints GitHub App installation tokens: short-lived (one
// hour) tokens scoped to an installation, and optionally narrowed to some of
// its repositories and permissions. The operator gives one to a sandboxed
// agent per run, so the App's private key never leaves the operator.
package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultAPIURL is the GitHub.com REST API.
const DefaultAPIURL = "https://api.github.com"

// Client mints installation tokens for one GitHub App installation.
type Client struct {
	AppID          string
	InstallationID int64
	Key            *rsa.PrivateKey
	// APIURL is the REST API base URL (default DefaultAPIURL; GitHub
	// Enterprise Server uses https://<host>/api/v3).
	APIURL string
	HTTP   *http.Client
	// Now is the clock (default time.Now); tests override it.
	Now func() time.Time
}

// New parses the App's PEM private key.
func New(appID string, installationID int64, privateKeyPEM []byte) (*Client, error) {
	if appID == "" || installationID == 0 {
		return nil, errors.New("github app: app ID and installation ID are required")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github app: parsing private key: %w", err)
	}
	return &Client{AppID: appID, InstallationID: installationID, Key: key}, nil
}

// TokenRequest narrows an installation token. Empty fields keep the
// installation's full scope, so callers should always narrow.
type TokenRequest struct {
	// Repositories are repository names without the owner (e.g. "agent-orca").
	Repositories []string `json:"repositories,omitempty"`
	// Permissions maps a permission to "read" or "write", e.g. contents: write.
	Permissions map[string]string `json:"permissions,omitempty"`
}

// APIError is a GitHub API error response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github app: %d %s", e.Status, e.Message)
}

// Permanent reports whether retrying cannot help: a 4xx other than rate limiting.
func (e *APIError) Permanent() bool {
	return e.Status >= 400 && e.Status < 500 && e.Status != http.StatusTooManyRequests && e.Status != http.StatusForbidden
}

// Token is a minted installation token.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) apiURL() string {
	if c.APIURL != "" {
		return strings.TrimSuffix(c.APIURL, "/")
	}
	return DefaultAPIURL
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// appJWT is the App's own credential: an RS256 JWT valid for ten minutes,
// back-dated a minute for clock skew, as GitHub recommends.
func (c *Client) appJWT() (string, error) {
	now := c.now()
	return jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    c.AppID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(c.Key)
}

// InstallationToken mints a token for the installation, narrowed by req.
func (c *Client) InstallationToken(ctx context.Context, req TokenRequest) (*Token, error) {
	appToken, err := c.appJWT()
	if err != nil {
		return nil, fmt.Errorf("github app: signing JWT: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", c.apiURL(), c.InstallationID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+appToken)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("github app: minting installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("minting installation token: %w", &APIError{Status: resp.StatusCode, Message: githubMessage(data)})
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
		return nil, fmt.Errorf("github app: unexpected token response")
	}
	return &Token{Value: out.Token, ExpiresAt: out.ExpiresAt}, nil
}

// RevokeToken revokes an installation token before it expires. A token that
// is already expired or revoked is not an error.
func (c *Client) RevokeToken(ctx context.Context, token string) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiURL()+"/installation/token", nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("github app: revoking token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusUnauthorized:
		return nil
	default:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("github app: revoking token: %s: %s", resp.Status, githubMessage(data))
	}
}

// githubMessage extracts GitHub's error message without echoing anything else
// from the response.
func githubMessage(data []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &e) == nil && e.Message != "" {
		return e.Message
	}
	return "(no message)"
}

// FromDir loads an App from a mounted Secret directory holding app-id,
// installation-id and private-key (PEM) files.
func FromDir(dir, apiURL string) (*Client, error) {
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("github app: reading %s: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	appID, err := read("app-id")
	if err != nil {
		return nil, err
	}
	inst, err := read("installation-id")
	if err != nil {
		return nil, err
	}
	installationID, err := strconv.ParseInt(inst, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("github app: installation-id %q is not a number", inst)
	}
	key, err := os.ReadFile(filepath.Join(dir, "private-key"))
	if err != nil {
		return nil, fmt.Errorf("github app: reading private-key: %w", err)
	}
	c, err := New(appID, installationID, key)
	if err != nil {
		return nil, err
	}
	c.APIURL = apiURL
	return c, nil
}
