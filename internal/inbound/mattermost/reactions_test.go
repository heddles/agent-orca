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

package mattermost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/inbound"
)

const postID = "postpostpostpostpostpost12"

func TestShowSwapsTheBotsReaction(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		if r.Header.Get("Authorization") != "Bearer "+botTok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + botID + `"}`))
	}))
	defer srv.Close()
	a, c := adapter(), conn(t, srv.URL, settingsDefault)
	to, _ := json.Marshal(target{Channel: chanID, Root: postID})
	for _, step := range [][2]inbound.Progress{
		{inbound.ProgressNone, inbound.ProgressWorking},
		{inbound.ProgressWorking, inbound.ProgressApproval},
		{inbound.ProgressApproval, inbound.ProgressDone},
	} {
		if err := a.Show(context.Background(), c, to, step[0], step[1]); err != nil {
			t.Fatal(err)
		}
	}
	add := func(name string) string {
		return `POST /api/v4/reactions {"emoji_name":"` + name + `","post_id":"` + postID + `","user_id":"` + botID + `"}`
	}
	remove := func(name string) string {
		return "DELETE /api/v4/users/" + botID + "/posts/" + postID + "/reactions/" + name
	}
	want := []string{
		"GET /api/v4/users/me", // once: the id is remembered
		add("eyes"),
		add("raised_hand"), remove("eyes"),
		add("white_check_mark"), remove("raised_hand"),
	}
	if !slices.Equal(calls, want) {
		t.Errorf("calls = %q\nwant %q", calls, want)
	}

	// Off, or a slash command with no post: nothing is called.
	calls = nil
	command, _ := json.Marshal(target{Channel: chanID})
	_ = a.Show(context.Background(), c, command, inbound.ProgressNone, inbound.ProgressWorking)
	c.Config = json.RawMessage(`{"workingReaction":false}`)
	_ = a.Show(context.Background(), c, to, inbound.ProgressNone, inbound.ProgressWorking)
	if len(calls) != 0 {
		t.Errorf("calls = %q", calls)
	}
}
