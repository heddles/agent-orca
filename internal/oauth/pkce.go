/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
)

// generatePKCE returns a (code_verifier, code_challenge) pair using the S256 method
// (SHA-256 of the verifier, base64url without padding), as required by the MCP OAuth
// metadata (code_challenge_methods_supported: ["S256"]).
func generatePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generating pkce verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// issuerOrigin returns the origin (scheme://host[:port]) of a URL, which is where
// an MCP server publishes its /.well-known/oauth-* discovery documents.
func issuerOrigin(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("url %q must have a scheme and host", rawURL)
	}
	return u.Scheme + "://" + u.Host, nil
}

// verifyPKCE validates that the S256 code_challenge corresponds to code_verifier.
// Used by tests; the running manager never needs to verify its own verifier.
func verifyPKCE(codeVerifier, codeChallenge string) bool {
	h := sha256.Sum256([]byte(codeVerifier))
	return base64.RawURLEncoding.EncodeToString(h[:]) == codeChallenge
}
