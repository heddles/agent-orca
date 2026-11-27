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

package inbound

// Messages nobody asked for in the chat: a schedule's report, posted through a
// shared chat app (docs/schedules.md).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/heddles/agent-orca/internal/redact"
)

// Poster is an adapter that can start a message in a place, not only reply.
type Poster interface {
	// Post sends text, already in the platform's markup, to a place.
	Post(ctx context.Context, c Connection, place, text string) error
}

const postTimeout = time.Minute

// sharedApp finds a tenant's shared app by name, ready to use.
func (p *Pipeline) sharedApp(ctx context.Context, tenantID, kind, app string) (Connection, Poster, error) {
	if tenantID == "" {
		return Connection{}, nil, errors.New("the tenant has no id, so it has no chat apps")
	}
	all, err := p.Store.ListInboundConnections(ctx, nil, "")
	if err != nil {
		return Connection{}, nil, errors.New("the chat apps could not be read")
	}
	for _, sc := range all {
		// The tenant is matched here whatever the store's own scope lets through.
		if !sc.Shared() || sc.TenantID != tenantID || sc.Kind != kind || sc.Name != app {
			continue
		}
		if !sc.Enabled {
			return Connection{}, nil, fmt.Errorf("the chat app %q is turned off", app)
		}
		adapter, ok := p.Registry.Get(kind)
		poster, can := adapter.(Poster)
		if !ok || !can {
			return Connection{}, nil, fmt.Errorf("%s apps cannot post on their own", kind)
		}
		c, err := p.Resolve(ctx, sc)
		if err != nil {
			return Connection{}, nil, fmt.Errorf("the credentials of the chat app %q cannot be used: %s", app, safeErr(err, c))
		}
		return c, poster, nil
	}
	return Connection{}, nil, fmt.Errorf("the tenant has no shared %s app named %q: an administrator connects it under Settings, Chat apps", kind, app)
}

// CheckApp reports whether a tenant's shared app can post. It does not call the
// platform. The error is safe to show.
func (p *Pipeline) CheckApp(ctx context.Context, tenantID, kind, app string) error {
	_, _, err := p.sharedApp(ctx, tenantID, kind, app)
	return err
}

// PostToApp posts to a place through a tenant's shared app. render makes the
// text and must mask the secrets it is given. The error is safe to show.
func (p *Pipeline) PostToApp(ctx context.Context, tenantID, kind, app, place string, render func(*redact.Registry) string) error {
	ctx, cancel := context.WithTimeout(ctx, postTimeout)
	defer cancel()
	c, poster, err := p.sharedApp(ctx, tenantID, kind, app)
	if err != nil {
		return err
	}
	if err := poster.Post(ctx, c, place, render(secretsOf(c))); err != nil {
		return fmt.Errorf("posting through the chat app %q failed: %s", app, safeErr(err, c))
	}
	return nil
}
