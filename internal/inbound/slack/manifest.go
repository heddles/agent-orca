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

package slack

import (
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Slack's limits on the names in a manifest.
const (
	maxAppName     = 35
	defaultAppName = "agent-orca"
)

// botScopes are what the created app asks for: to hear mentions and direct
// messages, to answer, to show it is working, and to name channels and people.
var botScopes = []string{
	"app_mentions:read", "chat:write", "im:history", "reactions:write", "channels:read", "groups:read", "users:read",
}

// SetupLink opens Slack's "create an app" page with a manifest filled in
// (https://docs.slack.dev/app-manifests/configuring-apps-with-app-manifests).
// Plain channel messages and slash commands are not asked for: they need more
// access, and can be added to the app later.
func (*Adapter) SetupLink(name, receiveURL string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = defaultAppName
	}
	if utf8.RuneCountInString(name) > maxAppName {
		name = string([]rune(name)[:maxAppName])
	}
	manifest := map[string]any{
		"display_information": map[string]any{"name": name},
		"features": map[string]any{
			"bot_user": map[string]any{"display_name": name, "always_online": true},
			"app_home": map[string]any{"messages_tab_enabled": true, "messages_tab_read_only_enabled": false},
		},
		"oauth_config": map[string]any{"scopes": map[string]any{"bot": botScopes}},
		"settings": map[string]any{
			"event_subscriptions": map[string]any{"request_url": receiveURL, "bot_events": []string{"app_mention", "message.im"}},
			"org_deploy_enabled":  false,
			"socket_mode_enabled": false,
		},
	}
	b, err := json.Marshal(manifest)
	if err != nil {
		return ""
	}
	return "https://api.slack.com/apps?" + url.Values{"new_app": {"1"}, "manifest_json": {string(b)}}.Encode()
}
