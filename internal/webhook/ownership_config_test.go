package webhook

import (
	"os"
	"strings"
	"testing"
)

// The ownership webhook guarantees nothing with failurePolicy: Ignore, so its
// entry must say Fail itself, not follow webhook.failurePolicy, in the chart and
// in config/webhook.
func TestOwnershipWebhookFailsClosed(t *testing.T) {
	for _, tc := range []struct{ file, start, end string }{
		{"../../charts/agent-orca/templates/00-webhook.yaml", "- name: vownership.kb.io", "---"},
		{"../../config/webhook/manifests.yaml", "name: vownership.kb.io", "- admissionReviewVersions"},
	} {
		b, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		i := strings.Index(s, tc.start)
		if i < 0 {
			t.Fatalf("%s has no ownership webhook", tc.file)
		}
		block := s[i+len(tc.start):]
		if j := strings.Index(block, tc.end); j >= 0 {
			block = block[:j]
		}
		if tc.file != "../../config/webhook/manifests.yaml" && !strings.Contains(block, "failurePolicy: Fail\n") {
			t.Errorf("%s: the ownership webhook is not failurePolicy: Fail:\n%s", tc.file, block)
		}
		if strings.Contains(block, ".Values.webhook.failurePolicy") {
			t.Errorf("%s: the ownership webhook follows webhook.failurePolicy", tc.file)
		}
	}
	// controller-gen puts failurePolicy before the name in the generated file.
	b, err := os.ReadFile("../../config/webhook/manifests.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "failurePolicy: Fail\n  name: vownership.kb.io") {
		t.Error("config/webhook: the ownership webhook is not failurePolicy: Fail")
	}
}
