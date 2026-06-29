/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package apiserver

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"golang.org/x/net/html"
)

// mcpAppCSPHeader returns a Content-Security-Policy that removes script-src
// 'unsafe-inline' (CWE-79) by requiring nonces on script/style elements injected
// by injectMCPAppCSPNonces.
func mcpAppCSPHeader(nonce string) string {
	return fmt.Sprintf(
		"default-src 'none'; script-src 'nonce-%s' 'strict-dynamic'; style-src 'nonce-%s' 'unsafe-inline'; img-src data: blob:; font-src data:; connect-src 'none'; frame-src 'none'; base-uri 'none'",
		nonce, nonce,
	)
}

func randomCSPNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// injectMCPAppCSPNonces parses HTML from an MCP App resource and adds a CSP
// nonce to every script and style element so a strict script-src can apply.
func injectMCPAppCSPNonces(fragment []byte, nonce string) ([]byte, error) {
	doc, err := html.Parse(bytes.NewReader(fragment))
	if err != nil {
		return nil, err
	}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style":
				setOrReplaceNonce(n, nonce)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func setOrReplaceNonce(n *html.Node, nonce string) {
	for i := range n.Attr {
		if n.Attr[i].Key == "nonce" {
			n.Attr[i].Val = nonce
			return
		}
	}
	n.Attr = append(n.Attr, html.Attribute{Key: "nonce", Val: nonce})
}
