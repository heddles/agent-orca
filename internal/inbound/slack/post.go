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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/heddles/agent-orca/internal/inbound"
	"github.com/heddles/agent-orca/internal/inbound/chat"
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

// Post sends a report to a channel the app is a member of: all of body,
// formatted, in as many messages as it takes, with header at the top of the
// first. Mentions stay inert and links are not unfurled. One report is posted to
// a channel at a time, so two do not interleave. Each call's retries are the
// HTTP client's, as for replies.
func (a *Adapter) Post(ctx context.Context, c inbound.Connection, place, header, body string) error {
	lock, _ := a.posting.LoadOrStore(c.ID+"/"+place, &sync.Mutex{})
	mu, _ := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	parts := a.reportParts(header, body)
	for i, text := range parts {
		var err error
		if i > 0 {
			err = a.pause(ctx, reportPause)
		}
		if err == nil {
			_, err = a.call(ctx, c, methodPostMessage, map[string]any{
				"channel": place, "text": text, "mrkdwn": true, "parse": "none", "link_names": false,
				"unfurl_links": false, "unfurl_media": false,
			}, nil)
		}
		if err == nil {
			continue
		}
		for code, help := range postHelp {
			if strings.HasSuffix(err.Error(), ": "+code) {
				err = fmt.Errorf("%w (%s)", err, help)
			}
		}
		if i > 0 {
			err = fmt.Errorf("only %d of %d messages were posted: %w", i, len(parts), err)
		}
		return err
	}
	return nil
}

// reportParts is the messages of a report. The header shares the first one when
// both fit.
func (a *Adapter) reportParts(header, body string) []string {
	caps := a.Capabilities()
	caps.MaxReplyParts = maxReportParts
	parts := chat.Render(body, caps)
	if len(parts) > 0 && utf8.RuneCountInString(header)+1+utf8.RuneCountInString(parts[0]) <= caps.MaxReplyChars {
		parts[0] = header + "\n" + parts[0]
		return parts
	}
	return append([]string{header}, parts...)
}

func (a *Adapter) pause(ctx context.Context, d time.Duration) error {
	if a.Pause != nil {
		return a.Pause(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
