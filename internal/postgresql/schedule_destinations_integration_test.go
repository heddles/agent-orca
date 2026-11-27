package postgresql

import (
	"context"
	"testing"
)

func TestScheduleDestinationsAgainstPostgres(t *testing.T) {
	s, _ := appStore(t)
	ctx := context.Background()
	a, b, plat := WithTenant(ctx, tenantA), WithTenant(ctx, tenantB), WithPlatform(ctx)
	d := ScheduleDestination{TenantID: tenantA, Namespace: evalNSA, Name: "digest", App: "bot", ChannelID: "C0ONE", ChannelName: "#one", SetBy: "ada"}
	if err := s.SaveScheduleDestination(a, d); err != nil {
		t.Fatal(err)
	}
	d.ChannelID, d.ChannelName, d.SetBy = "C0TWO", "#two", "grace"
	if err := s.SaveScheduleDestination(a, d); err != nil {
		t.Fatal(err)
	}
	for who, c := range map[string]context.Context{"owner": a, "platform": plat} {
		got, found, err := s.GetScheduleDestination(c, tenantA, evalNSA, "digest")
		if err != nil || !found || got.ChannelID != "C0TWO" || got.SetBy != "grace" || got.SetAt.IsZero() {
			t.Fatalf("%s: %+v %v %v", who, got, found, err)
		}
	}
	// Another tenant sees, changes and removes nothing, even by naming the owner.
	if _, found, err := s.GetScheduleDestination(b, tenantA, evalNSA, "digest"); err != nil || found {
		t.Errorf("tenant B read it: %v %v", found, err)
	}
	if list, err := s.ListScheduleDestinations(b); err != nil || len(list) != 0 {
		t.Errorf("tenant B listed %v %v", list, err)
	}
	if err := s.SaveScheduleDestination(b, d); err == nil {
		t.Error("tenant B saved a row of tenant A")
	}
	if gone, err := s.DeleteScheduleDestination(b, tenantA, evalNSA, "digest"); err != nil || gone {
		t.Errorf("tenant B removed it: %v %v", gone, err)
	}
	if err := s.DeleteScheduleDestinationsOf(b, evalNSA, "digest"); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListScheduleDestinations(a); err != nil || len(list) != 1 {
		t.Fatalf("list = %v %v", list, err)
	}
	if gone, err := s.DeleteScheduleDestination(a, tenantA, evalNSA, "digest"); err != nil || !gone {
		t.Fatalf("remove = %v %v", gone, err)
	}
	if err := s.SaveScheduleDestination(a, d); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteScheduleDestinationsOf(plat, evalNSA, "digest"); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListScheduleDestinations(plat); err != nil || len(list) != 0 {
		t.Errorf("after the schedule was deleted: %v %v", list, err)
	}
}
