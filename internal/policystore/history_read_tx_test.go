package policystore

import (
	"github.com/leileipei/Enterprise_IM/internal/policy"
	"testing"
	"time"
)

func TestHistoryFinalDirectVisibility(t *testing.T) {
	at := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
	reader := policy.Membership{ID: "reader", TenantID: "tenant", OrganizationID: "org"}
	peer := policy.Membership{ID: "peer", TenantID: "tenant", OrganizationID: "org"}
	scope := historyReadContext{Actor: reader, Retention: time.Hour}
	batch := historyReadBatch{Page: MessagePage{Messages: []PulledMessage{{Seq: 7, Text: "secret", ServerTime: at}}}, Direct: []historicalPair{{reader: reader, peer: peer}}}
	if got := filterDirectHistoryBatch(scope, batch, at.Add(time.Hour-time.Nanosecond)); got.Messages[0].Text != "secret" {
		t.Fatalf("before expiry %+v", got)
	}
	if got := filterDirectHistoryBatch(scope, batch, at.Add(time.Hour)); !got.Messages[0].Redacted || got.Messages[0].Text != "" || got.Messages[0].Seq != 7 {
		t.Fatalf("at expiry %+v", got)
	}
	scope.Rules = []policy.Rule{{ID: "hard", TenantID: "tenant", Effect: policy.EffectHardDeny, Action: policy.ActionSendMessage, SourceOrganizationID: "org", TargetOrganizationID: "org", EffectiveFrom: at.Add(time.Second)}}
	if got := filterDirectHistoryBatch(scope, batch, at.Add(time.Second)); !got.Messages[0].Redacted {
		t.Fatalf("policy effective %+v", got)
	}
	// A final filtering pass must not mutate the retained evidence or an earlier result.
	if batch.Page.Messages[0].Text != "secret" {
		t.Fatal("final filtering mutated evidence")
	}
}

func TestHistoryBatchKeepsUnauthorizedEvidencePrivate(t *testing.T) {
	at := time.Now().UTC()
	scope := historyReadContext{Actor: policy.Membership{ID: "reader", TenantID: "tenant"}, Retention: time.Hour}
	for _, evidence := range [][]historicalPair{nil, {{reader: policy.Membership{ID: "reader", TenantID: "tenant"}, peer: policy.Membership{ID: "peer", TenantID: "other"}}}} {
		batch := historyReadBatch{Page: MessagePage{Messages: []PulledMessage{{Seq: 9, Text: "hidden", ServerTime: at}}}, Direct: evidence}
		got := filterDirectHistoryBatch(scope, batch, at)
		if !got.Messages[0].Redacted || got.Messages[0].Text != "" || got.Messages[0].Seq != 9 {
			t.Fatalf("invalid evidence %+v", got)
		}
	}
}
