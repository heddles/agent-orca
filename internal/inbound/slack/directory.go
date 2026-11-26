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
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/heddles/agent-orca/internal/inbound"
)

const (
	directoryPageSize = "200"
	directoryMaxPages = 5
	directoryTimeout  = 20 * time.Second
)

// ListPlaces lists the channels the app can see (needs channels:read, and
// groups:read for private ones), those the bot is in first. At most a thousand.
func (a *Adapter) ListPlaces(ctx context.Context, c inbound.Connection) ([]inbound.Place, error) {
	ctx, cancel := context.WithTimeout(ctx, directoryTimeout)
	defer cancel()
	var out []inbound.Place
	cursor := ""
	for range directoryMaxPages {
		var page struct {
			Channels []struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				IsMember bool   `json:"is_member"`
			} `json:"channels"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		args := url.Values{"types": {"public_channel,private_channel"}, "exclude_archived": {"true"}, "limit": {directoryPageSize}}
		if cursor != "" {
			args.Set("cursor", cursor)
		}
		if err := a.form(ctx, c, "conversations.list", args, &page); err != nil {
			return nil, err
		}
		for _, ch := range page.Channels {
			out = append(out, inbound.Place{ID: ch.ID, Name: "#" + ch.Name, Member: ch.IsMember})
		}
		if cursor = page.Metadata.NextCursor; cursor == "" {
			break
		}
	}
	slices.SortStableFunc(out, func(x, y inbound.Place) int {
		if x.Member != y.Member {
			if x.Member {
				return -1
			}
			return 1
		}
		return strings.Compare(x.Name, y.Name)
	})
	return out, nil
}

// PersonName is a member's display name (needs users:read).
func (a *Adapter) PersonName(ctx context.Context, c inbound.Connection, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, directoryTimeout)
	defer cancel()
	var who struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
		} `json:"user"`
	}
	if err := a.form(ctx, c, "users.info", url.Values{"user": {id}}, &who); err != nil {
		return "", err
	}
	if who.User.RealName != "" {
		return who.User.RealName, nil
	}
	return who.User.Name, nil
}
