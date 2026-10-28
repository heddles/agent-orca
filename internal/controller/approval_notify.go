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

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/postgresql"
)

// ApprovalNotifier tells approvers about a request waiting for them.
type ApprovalNotifier interface {
	NotifyApproval(ctx context.Context, a postgresql.Approval) error
}

// TextNotifier posts a plain message to the same channel as approvals.
type TextNotifier interface {
	NotifyText(ctx context.Context, text string) error
}

// WebhookApprovalNotifier posts each new pending approval to an incoming
// webhook. The body is Slack's incoming-webhook format ({"text": ...}) plus the
// approval itself under "approval", so other receivers can use the fields.
type WebhookApprovalNotifier struct {
	URL string
	// UIURL is the UI's public base URL; messages link to its approvals inbox.
	UIURL string
	HTTP  *http.Client
}

// NotifyApproval implements ApprovalNotifier.
func (n *WebhookApprovalNotifier) NotifyApproval(ctx context.Context, a postgresql.Approval) error {
	return n.post(ctx, map[string]any{"text": approvalMessage(a, n.UIURL), "approval": a})
}

// NotifyText implements TextNotifier.
func (n *WebhookApprovalNotifier) NotifyText(ctx context.Context, text string) error {
	return n.post(ctx, map[string]any{"text": text})
}

func (n *WebhookApprovalNotifier) post(ctx context.Context, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := n.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posting notification: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("posting notification: %s", resp.Status)
	}
	return nil
}

// approvalMessage is the notification text. It names the run and what was
// asked for, never secrets: approvals hold none.
func approvalMessage(a postgresql.Approval, uiURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, ":warning: Approval needed in %s for run %s: %s.", a.Namespace, a.RunName, a.Summary)
	if len(a.ApproverGroups) > 0 {
		fmt.Fprintf(&b, " Approvers: %s.", strings.Join(a.ApproverGroups, ", "))
	}
	fmt.Fprintf(&b, " Rejected automatically at %s if nobody decides.", a.ExpiresAt.UTC().Format("15:04 MST"))
	if uiURL != "" {
		fmt.Fprintf(&b, " <%s/?tab=approvals|Review it>", strings.TrimSuffix(uiURL, "/"))
	}
	return b.String()
}
