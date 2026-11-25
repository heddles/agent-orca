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
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDirectoryNamesChannelsAndPeople(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("these methods take form arguments: %v %s", err, r.Header.Get("Content-Type"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/conversations.list") && r.PostForm.Get("cursor") == "":
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C2","name":"zeta","is_member":false}],"response_metadata":{"next_cursor":"n"}}`))
		case strings.HasSuffix(r.URL.Path, "/conversations.list"):
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C1","name":"ops","is_member":true}]}`))
		case strings.HasSuffix(r.URL.Path, "/users.info") && r.PostForm.Get("user") == "U1":
			_, _ = w.Write([]byte(`{"ok":true,"user":{"name":"sam","real_name":"Sam Doe"}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"missing_scope"}`))
		}
	}))
	defer srv.Close()
	a, c := adapter(srv.URL), conn(t, defaultSettings)
	places, err := a.ListPlaces(context.Background(), c)
	if err != nil || len(places) != 2 || places[0].Name != "#ops" || !places[0].Member || places[1].ID != "C2" {
		t.Fatalf("places = %+v, %v (both pages, channels with the bot first)", places, err)
	}
	if name, err := a.PersonName(context.Background(), c, "U1"); err != nil || name != "Sam Doe" {
		t.Errorf("name = %q, %v", name, err)
	}
	if _, err := a.PersonName(context.Background(), c, "U2"); err == nil || !strings.Contains(err.Error(), "missing_scope") {
		t.Errorf("err = %v", err)
	}
}
