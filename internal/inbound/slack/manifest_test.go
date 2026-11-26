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
	"slices"
	"strings"
	"testing"
)

func TestSetupLinkFillsInTheAppSettings(t *testing.T) {
	const receive = "https://hooks.example.com/inbound/slack/inb_x"
	u, err := url.Parse((&Adapter{}).SetupLink(strings.Repeat("é", 50), receive))
	if err != nil || u.Host != "api.slack.com" || u.Query().Get("new_app") != "1" {
		t.Fatalf("url = %v, %v", u, err)
	}
	var m struct {
		Display  struct{ Name string } `json:"display_information"`
		Settings struct {
			Events struct {
				URL    string   `json:"request_url"`
				Events []string `json:"bot_events"`
			} `json:"event_subscriptions"`
			Socket bool `json:"socket_mode_enabled"`
		} `json:"settings"`
		OAuth struct {
			Scopes struct{ Bot []string } `json:"scopes"`
		} `json:"oauth_config"`
	}
	if err := json.Unmarshal([]byte(u.Query().Get("manifest_json")), &m); err != nil {
		t.Fatal(err)
	}
	if m.Settings.Events.URL != receive || m.Settings.Socket || len([]rune(m.Display.Name)) != maxAppName {
		t.Errorf("manifest = %+v", m)
	}
	if !slices.Contains(m.Settings.Events.Events, "app_mention") || !slices.Contains(m.OAuth.Scopes.Bot, "chat:write") {
		t.Errorf("manifest = %+v", m)
	}
	for _, wide := range []string{"channels:history", "groups:history", "commands"} {
		if slices.Contains(m.OAuth.Scopes.Bot, wide) {
			t.Errorf("the app asks for %s, which the default setup does not need", wide)
		}
	}
}
