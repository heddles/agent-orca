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
	"fmt"
	"strings"

	"github.com/heddles/agent-orca/internal/inbound"
)

var _ inbound.Poster = (*Adapter)(nil)

// methodPostMessage is the Web API method that posts a message.
const methodPostMessage = "chat.postMessage"

// postHelp says what to do about the refusals a person can fix.
var postHelp = map[string]string{
	"not_in_channel":    "invite the app to the channel",
	"channel_not_found": "check the channel id, and invite the app if the channel is private",
	"is_archived":       "the channel is archived",
}

// Post sends a message to a channel the app is a member of. text is mrkdwn that
// the caller has already escaped. Mentions stay inert and links are not
// unfurled. Retries are the HTTP client's, as for replies.
func (a *Adapter) Post(ctx context.Context, c inbound.Connection, place, text string) error {
	_, err := a.call(ctx, c, methodPostMessage, map[string]any{
		"channel": place, "text": text, "mrkdwn": true, "parse": "none", "link_names": false,
		"unfurl_links": false, "unfurl_media": false,
	}, nil)
	if err != nil {
		for code, help := range postHelp {
			if strings.HasSuffix(err.Error(), ": "+code) {
				return fmt.Errorf("%w (%s)", err, help)
			}
		}
	}
	return err
}
