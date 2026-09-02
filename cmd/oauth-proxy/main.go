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

// Command oauth-proxy is a generic, metadata-driven OAuth 2.0 credential manager
// for remote MCP servers. OAuth-only MCP servers (e.g. Slack's public MCP at
// https://mcp.slack.com/mcp) reject hand-pasted bearer tokens, so this sidecar
// discovers each server's OAuth config from its RFC 8414 metadata, exchanges a
// refresh token (or a one-time authorization code via PKCE) for an access token, and
// writes the access token into a Kubernetes Secret that an MCPServer's
// spec.auth.bearerToken already points the model-router at.
//
// One binary serves any OAuth-only MCP — only the per-server Secret differs:
//
//	oauth-proxy login-url --server-url ... --client-id ... --redirect-uri ...
//	oauth-proxy seed      --server-url ... --oauth-secret ns/creds --code ... --code-verifier ...
//	oauth-proxy run       --server-url ... --oauth-secret ns/creds --access-token-secret ns/slack-mcp-token
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/floppyfish14/agent-orca/internal/oauth"
)

const (
	accessKey      = "token" // key the model-router reads as the bearer
	clientIDKey    = "client_id"
	secretKeyKey   = "client_secret"
	redirectURIKey = "redirect_uri"
	refreshKey     = "refresh_token"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "run":
		os.Exit(runCmd(ctx, os.Args[2:]))
	case "seed":
		os.Exit(seedCmd(ctx, os.Args[2:]))
	case "login-url":
		os.Exit(loginURLCmd(ctx, os.Args[2:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: oauth-proxy <run|seed|login-url> [flags]")
}

func splitNsName(s string) (ns, name string, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("expected <namespace/name>, got %q", s)
	}
	return parts[0], parts[1], nil
}

func commaScopes(s string) []string {
	var out []string
	for _, sc := range strings.Split(s, ",") {
		if sc = strings.TrimSpace(sc); sc != "" {
			out = append(out, sc)
		}
	}
	return out
}

func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// k8sClient builds an in-cluster clientset (the sidecar always runs as a pod).
func k8sClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return kubernetes.NewForConfig(cfg)
}

// readCreds reads the OAuth client credentials (and optional refresh token and
// redirect URI) from the oauth creds Secret.
func readCreds(secrets oauth.SecretIO, ns, name string) (clientID, clientSecret, refreshToken, redirectURI string, err error) {
	get := func(key string) (string, bool, error) {
		v, ok, e := secrets.ReadKey(context.Background(), ns, name, key)
		return string(v), ok, e
	}
	if clientID, _, err = get(clientIDKey); err != nil || clientID == "" {
		err = fmt.Errorf("secret %s/%s missing key %q", ns, name, clientIDKey)
		return
	}
	if clientSecret, _, err = get(secretKeyKey); err != nil || clientSecret == "" {
		err = fmt.Errorf("secret %s/%s missing key %q", ns, name, secretKeyKey)
		return
	}
	if v, ok, e := get(refreshKey); e != nil {
		err = e
		return
	} else if ok {
		refreshToken = string(v)
	}
	if v, ok, e := get(redirectURIKey); e != nil {
		err = e
		return
	} else if ok {
		redirectURI = string(v)
	}
	return
}

// loginURLCmd prints the browser authorization URL + PKCE verifier for the one-time
// exchange. Runs locally (no cluster access needed); creds come via flags.
func loginURLCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("login-url", flag.ExitOnError)
	var serverURL, clientID, redirectURI, scopes string
	fs.StringVar(&serverURL, "server-url", "", "MCP server URL (required)")
	fs.StringVar(&clientID, "client-id", "", "OAuth client id (required)")
	fs.StringVar(&redirectURI, "redirect-uri", "", "registered redirect URI (required)")
	fs.StringVar(&scopes, "scopes", "", "comma-separated OAuth scopes")
	_ = fs.Parse(args)
	logger := slog.Default()
	if serverURL == "" || clientID == "" {
		fs.Usage()
		return 2
	}

	mgr := oauth.New(httpClient(), nil)
	meta, err := mgr.Discover(ctx, serverURL)
	if err != nil {
		logger.Error("discovery failed", "err", err)
		return 1
	}
	cfg := &oauth.Config{ServerURL: serverURL, ClientID: clientID, RedirectURI: redirectURI, Scopes: commaScopes(scopes)}
	authURL, verifier, err := mgr.LoginURL(meta, cfg)
	if err != nil {
		logger.Error("building login URL", "err", err)
		return 1
	}
	fmt.Println("Open this URL in a browser to authorize the MCP OAuth client:")
	fmt.Println(authURL)
	fmt.Println()
	fmt.Println("Paste the resulting `code` and THIS code_verifier into:")
	fmt.Printf("  oauth-proxy seed --server-url %s --oauth-secret <ns/creds> --code <code> --code-verifier %s\n", serverURL, verifier)
	return 0
}

// seedCmd performs the one-time authorization_code (PKCE) exchange and writes both the
// refresh token (into the oauth creds Secret) and the access token (into the
// access-token Secret referenced by MCPServer.auth.bearerToken).
func seedCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	var serverURL, oauthSecret, accessTokenSec, code, codeVerifier, scopes string
	fs.StringVar(&serverURL, "server-url", "", "MCP server URL (required)")
	fs.StringVar(&oauthSecret, "oauth-secret", "", "Secret with client_id/client_secret/redirect_uri (namespace/name) (required)")
	fs.StringVar(&accessTokenSec, "access-token-secret", "", "Secret to write the access token into (namespace/name) (required)")
	fs.StringVar(&code, "code", "", "authorization code from the callback (required)")
	fs.StringVar(&codeVerifier, "code-verifier", "", "PKCE code_verifier from login-url (required)")
	fs.StringVar(&scopes, "scopes", "", "comma-separated OAuth scopes")
	_ = fs.Parse(args)
	logger := slog.Default()
	if serverURL == "" || oauthSecret == "" || accessTokenSec == "" || code == "" || codeVerifier == "" {
		fs.Usage()
		return 2
	}

	oauthNS, oauthName, err := splitNsName(oauthSecret)
	if err != nil {
		logger.Error("parsing oauth-secret", "err", err)
		return 2
	}
	atNS, atName, err := splitNsName(accessTokenSec)
	if err != nil {
		logger.Error("parsing access-token-secret", "err", err)
		return 2
	}

	secrets, err := k8sClient()
	if err != nil {
		logger.Error("building kubernetes client", "err", err)
		return 1
	}
	secIO := oauth.NewK8sSecrets(secrets)

	clientID, clientSecret, _, redirectURI, err := readCreds(secIO, oauthNS, oauthName)
	if err != nil {
		logger.Error("reading oauth creds", "err", err)
		return 1
	}

	mgr := oauth.New(httpClient(), secIO)
	meta, err := mgr.Discover(ctx, serverURL)
	if err != nil {
		logger.Error("discovery failed", "err", err)
		return 1
	}
	cfg := &oauth.Config{
		ServerURL:            serverURL,
		ClientID:             clientID,
		ClientSecret:         clientSecret,
		RedirectURI:          redirectURI,
		Scopes:               commaScopes(scopes),
		AccessTokenSecretRef: oauth.SecretKeyRef{Namespace: atNS, Name: atName, Key: accessKey},
	}
	resp, err := mgr.ExchangeCode(ctx, meta, cfg, code, codeVerifier)
	if err != nil {
		logger.Error("exchange failed", "err", err)
		return 1
	}
	if resp.RefreshToken != "" {
		if err := secIO.WriteKey(ctx, oauthNS, oauthName, refreshKey, []byte(resp.RefreshToken)); err != nil {
			logger.Error("writing refresh token", "err", err)
			return 1
		}
	}
	if err := secIO.WriteKey(ctx, atNS, atName, accessKey, []byte(resp.AccessToken)); err != nil {
		logger.Error("writing access token", "err", err)
		return 1
	}
	logger.Info("seeded oauth tokens", "access-token-secret", atName, "has-refresh", resp.RefreshToken != "")
	return 0
}

// runCmd is the long-lived sidecar: discovers the server, then refreshes the access
// token on a cadence and rewrites the access-token Secret. The refresh token is
// re-read from its Secret each cycle (inside oauth.TokenManager.Run) so rotations
// persist across restarts.
func runCmd(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var serverURL, oauthSecret, accessTokenSec, scopes string
	var refreshInterval time.Duration
	fs.StringVar(&serverURL, "server-url", "", "MCP server URL (required)")
	fs.StringVar(&oauthSecret, "oauth-secret", "", "Secret with client_id/client_secret/refresh_token/redirect_uri (namespace/name) (required)")
	fs.StringVar(&accessTokenSec, "access-token-secret", "", "Secret to write the access token into (namespace/name) (required)")
	fs.StringVar(&scopes, "scopes", "", "comma-separated OAuth scopes")
	fs.DurationVar(&refreshInterval, "refresh-interval", 50*time.Minute, "refresh cadence when no expires_in known")
	_ = fs.Parse(args)
	logger := slog.Default()
	if serverURL == "" || oauthSecret == "" || accessTokenSec == "" {
		fs.Usage()
		return 2
	}

	oauthNS, oauthName, err := splitNsName(oauthSecret)
	if err != nil {
		logger.Error("parsing oauth-secret", "err", err)
		return 2
	}
	atNS, atName, err := splitNsName(accessTokenSec)
	if err != nil {
		logger.Error("parsing access-token-secret", "err", err)
		return 2
	}

	clientset, err := k8sClient()
	if err != nil {
		logger.Error("building kubernetes client", "err", err)
		return 1
	}
	secIO := oauth.NewK8sSecrets(clientset)

	mgr := oauth.New(httpClient(), secIO)
	meta, err := mgr.Discover(ctx, serverURL)
	if err != nil {
		logger.Error("discovery failed", "err", err, "server", serverURL)
		return 1
	}

	clientID, clientSecret, refreshToken, redirectURI, err := readCreds(secIO, oauthNS, oauthName)
	if err != nil {
		logger.Error("reading oauth creds", "err", err)
		return 1
	}
	cfg := &oauth.Config{
		ServerURL:             serverURL,
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		RedirectURI:           redirectURI,
		Scopes:                commaScopes(scopes),
		RefreshToken:          refreshToken,
		RefreshTokenSecretRef: oauth.SecretKeyRef{Namespace: oauthNS, Name: oauthName, Key: refreshKey},
		AccessTokenSecretRef:  oauth.SecretKeyRef{Namespace: atNS, Name: atName, Key: accessKey},
		RefreshInterval:       refreshInterval,
	}
	if err := mgr.Run(ctx, meta, cfg); err != nil {
		logger.Error("oauth run exited", "err", err)
		if ctx.Err() != nil {
			return 0
		}
		return 1
	}
	logger.Info("oauth proxy shutting down")
	return 0
}
