package inbound

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/heddles/agent-orca/internal/postgresql"
	"github.com/heddles/agent-orca/internal/redact"
)

type postingAdapter struct {
	*fakeAdapter
	places, texts []string
	fail          error
}

func (p *postingAdapter) Post(_ context.Context, _ Connection, place, header, body string) error {
	p.places, p.texts = append(p.places, place), append(p.texts, header+" "+body)
	return p.fail
}

func TestPostToAppStaysInsideTheTenant(t *testing.T) {
	const token = "xoxb-a-very-secret-token"
	f := newFixture(t)
	ad := &postingAdapter{fakeAdapter: f.ad}
	reg := NewRegistry()
	reg.Register(ad)
	f.p.Registry = reg
	f.p.Secrets = func(context.Context, string, string) (map[string][]byte, error) {
		return map[string][]byte{"botToken": []byte(token)}, nil
	}
	app := func(id, tenant, name string, enabled bool) {
		f.st.conns[id] = postgresql.InboundConnection{ID: id, TenantID: tenant, Kind: "fake", Name: name, Enabled: enabled, SecretRef: "s", Config: []byte(`{}`)}
	}
	app("a", "t-1", "orca-bot", true)
	app("b", "t-2", "other-bot", true)
	app("c", "t-1", "off-bot", false)
	// An agent's own channel is not a shared app, whatever its name.
	f.st.conns["d"] = postgresql.InboundConnection{ID: "d", TenantID: "t-1", Kind: "fake", Name: "own-channel", Namespace: "ns", Agent: "a", Enabled: true}
	render := func(r *redact.Registry) (string, string) { return "report", r.String("with " + token) }
	ctx := context.Background()

	for name, tc := range map[string]struct{ tenant, app, want string }{
		"another tenant's app": {"t-1", "other-bot", "no shared"},
		"no tenant id":         {"", "orca-bot", "no id"},
		"turned off":           {"t-1", "off-bot", "turned off"},
		"an agent's channel":   {"t-1", "own-channel", "no shared"},
		"unknown":              {"t-1", "nope", "no shared"},
	} {
		if err := f.p.PostToApp(ctx, tc.tenant, "fake", tc.app, "C1", render); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
		if err := f.p.CheckApp(ctx, tc.tenant, "fake", tc.app); err == nil {
			t.Errorf("%s: CheckApp passed", name)
		}
	}
	if len(ad.places) != 0 {
		t.Fatalf("posted: %v", ad.places)
	}

	if err := f.p.PostToApp(ctx, "t-1", "fake", "orca-bot", "C1", render); err != nil {
		t.Fatal(err)
	}
	if len(ad.places) != 1 || ad.places[0] != "C1" || strings.Contains(ad.texts[0], token) {
		t.Errorf("places = %v, texts = %q", ad.places, ad.texts)
	}
	ad.fail = errors.New("refused for " + token)
	if err := f.p.PostToApp(ctx, "t-1", "fake", "orca-bot", "C1", render); err == nil || strings.Contains(err.Error(), token) {
		t.Errorf("the error must not hold the token: %v", err)
	}
}
