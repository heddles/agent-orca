package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// aoctl schedule talks to the UI API (:8083), where schedules live, with the
// same bearer token as the other commands (the UI API accepts a tenant token).

const schedulesPath = "/api/schedules"

const defaultUIEndpoint = "http://localhost:8083"

// scheduleSummary is the part of GET /api/schedules aoctl shows.
type scheduleSummary struct {
	Name        string `json:"name"`
	Namespace   string `json:"namespace"`
	Target      string `json:"target"`
	TargetKind  string `json:"targetKind"`
	Schedule    string `json:"schedule"`
	TimeZone    string `json:"timeZone"`
	State       string `json:"state"`
	StateReason string `json:"stateReason"`
	NextRun     string `json:"nextRun"`
	LastResult  struct {
		Status string `json:"status"`
		At     string `json:"at"`
		Reason string `json:"reason"`
	} `json:"lastResult"`
	SpentTodayUSD string `json:"spentTodayUSD"`
	SpentMonthUSD string `json:"spentMonthUSD"`
	Ownership     struct {
		Owner string `json:"owner"`
	} `json:"ownership"`
}

func (c *Client) uiBase() string {
	if c.UI != "" {
		return strings.TrimRight(c.UI, "/")
	}
	return defaultUIEndpoint
}

// uiCall sends a request to the UI API and decodes a JSON answer into out.
func (c *Client) uiCall(ctx context.Context, method, path string, wantStatus int, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.uiBase()+path, nil)
	if err != nil {
		return err
	}
	c.tokenAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeJSON(resp, out, wantStatus)
}

func schedulePath(ns, name, action string) string {
	return fmt.Sprintf("/api/schedules/%s/%s/%s", url.PathEscape(ns), url.PathEscape(name), action)
}

func newScheduleCommand(s *settings) *cobra.Command {
	var ns string
	cmd := &cobra.Command{Use: "schedule", Short: "Scheduled runs (UI API)",
		Long: "List and control AgentSchedules. Create and edit them in the UI or in Git; see docs/schedules.md."}
	cmd.PersistentFlags().StringVar(&s.ui, "ui-endpoint", "",
		"UI API base URL (default: $AOCTL_UI_ENDPOINT or "+defaultUIEndpoint+")")
	cmd.PersistentFlags().StringVarP(&ns, "namespace", "n", "", "namespace (default: the tenant's own)")

	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List schedules with their next run and last result",
		RunE: func(_ *cobra.Command, _ []string) error {
			var list []scheduleSummary
			path := schedulesPath
			if ns != "" {
				path += "?namespace=" + url.QueryEscape(ns)
			}
			if err := s.client().uiCall(context.Background(), http.MethodGet, path, http.StatusOK, &list); err != nil {
				return err
			}
			return s.render(list, func(w io.Writer) error {
				if len(list) == 0 {
					_, _ = fmt.Fprintln(w, "No schedules found.")
					return nil
				}
				tw := newTableWriter().header("NAME", "NAMESPACE", "TARGET", "SCHEDULE", "STATE", "NEXT RUN", "LAST RESULT")
				for _, g := range list {
					tw.row(g.Name, g.Namespace, g.Target, g.Schedule+" "+g.TimeZone, g.State, g.NextRun, g.LastResult.Status)
				}
				return tw.render(w)
			})
		},
	})

	find := func(name string) (*scheduleSummary, error) {
		var list []scheduleSummary
		path := schedulesPath
		if ns != "" {
			path += "?namespace=" + url.QueryEscape(ns)
		}
		if err := s.client().uiCall(context.Background(), http.MethodGet, path, http.StatusOK, &list); err != nil {
			return nil, err
		}
		var hit *scheduleSummary
		for i := range list {
			if list[i].Name == name {
				if hit != nil {
					return nil, fmt.Errorf("schedule %q exists in several namespaces: pass --namespace", name)
				}
				hit = &list[i]
			}
		}
		if hit == nil {
			return nil, fmt.Errorf("schedule %q not found", name)
		}
		return hit, nil
	}
	cmd.AddCommand(&cobra.Command{
		Use: "get <name>", Short: "Show one schedule", Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			g, err := find(args[0])
			if err != nil {
				return err
			}
			return s.render(g, func(w io.Writer) error {
				_, _ = fmt.Fprintf(w, "Name:        %s/%s (owner: %s)\nTarget:      %s %s\n"+
					"Schedule:    %s (%s)\nState:       %s %s\nNext run:    %s\n"+
					"Last result: %s %s %s\nSpent:       $%s today, $%s this month\n",
					g.Namespace, g.Name, g.Ownership.Owner, g.TargetKind, g.Target, g.Schedule, g.TimeZone, g.State, g.StateReason,
					g.NextRun, g.LastResult.Status, g.LastResult.At, g.LastResult.Reason, g.SpentTodayUSD, g.SpentMonthUSD)
				return nil
			})
		},
	})
	action := func(use, short, verb, apiAction string, want int) *cobra.Command {
		return &cobra.Command{
			Use: use + " <name>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				g, err := find(args[0])
				if err != nil {
					return err
				}
				var out map[string]string
				path := schedulePath(g.Namespace, g.Name, apiAction)
				if err := s.client().uiCall(context.Background(), http.MethodPost, path, want, &out); err != nil {
					return err
				}
				return s.render(out, func(w io.Writer) error {
					if n := out["name"]; n != "" {
						_, _ = fmt.Fprintf(w, "%s %s/%s: %s %s\n", verb, g.Namespace, g.Name, out["kind"], n)
						return nil
					}
					_, _ = fmt.Fprintf(w, "%s %s/%s\n", verb, g.Namespace, g.Name)
					return nil
				})
			},
		}
	}
	cmd.AddCommand(
		action("suspend", "Stop new runs (a Git-owned schedule is suspended in Git)", "suspended", "suspend", http.StatusOK),
		action("resume", "Resume a suspended or degraded schedule", "resumed", "resume", http.StatusOK),
		action("run", "Run the schedule now (same quotas as a tick)", "started", "run", http.StatusAccepted),
	)
	return cmd
}
