package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

type groupOwnerTransferStub struct {
	conversationStub
	transfer func(context.Context, access.TrustedIdentity, string, policystore.GroupOwnerTransferRequest) (policystore.GroupOwnerTransfer, error)
}

func (s groupOwnerTransferStub) TransferGroupOwner(ctx context.Context, id access.TrustedIdentity, group string, req policystore.GroupOwnerTransferRequest) (policystore.GroupOwnerTransfer, error) {
	return s.transfer(ctx, id, group, req)
}

func groupOwnerTransferRequest(body string) *http.Request {
	r := adminRequest(http.MethodPost, "/api/v1/groups/"+targetUserID+"/owner-transfers")
	r.Header.Set("Content-Type", "application/json")
	r.Body = io.NopCloser(strings.NewReader(body))
	return r
}

const ownerTransferBody = `{"client_request_id":"00000000-0000-4000-8000-000000000901","source_interval_id":"` + groupIntervalID + `","target_interval_id":"` + targetMemID + `"}`

func TestGroupOwnerTransferRouteUsesTrustedIdentityAndReturnsCreatedOrReplay(t *testing.T) {
	created := true
	service := groupOwnerTransferStub{transfer: func(_ context.Context, id access.TrustedIdentity, group string, req policystore.GroupOwnerTransferRequest) (policystore.GroupOwnerTransfer, error) {
		if id != (access.TrustedIdentity{TenantID: tenantID, UserID: actorID, ActingMembershipID: actingID}) || group != targetUserID ||
			req.ClientRequestID != "00000000-0000-4000-8000-000000000901" || req.SourceIntervalID != groupIntervalID || req.TargetIntervalID != targetMemID {
			t.Fatalf("untrusted transfer input: %+v %s %+v", id, group, req)
		}
		return policystore.GroupOwnerTransfer{SourceIntervalID: groupIntervalID, TargetIntervalID: targetMemID, Created: created}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), service)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []int{http.StatusCreated, http.StatusOK} {
		req := groupOwnerTransferRequest(ownerTransferBody)
		req.Header.Set("X-Tenant-ID", "99999999-9999-4999-8999-999999999999")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != expected || !strings.Contains(res.Body.String(), `"source_interval_id":"`+groupIntervalID+`"`) ||
			!strings.Contains(res.Body.String(), `"target_interval_id":"`+targetMemID+`"`) {
			t.Fatalf("transfer response: %d %s", res.Code, res.Body.String())
		}
		created = false
	}
}

func TestGroupOwnerTransferRouteRejectsMalformedAndMapsErrors(t *testing.T) {
	notCalled := groupOwnerTransferStub{transfer: func(context.Context, access.TrustedIdentity, string, policystore.GroupOwnerTransferRequest) (policystore.GroupOwnerTransfer, error) {
		t.Fatal("transfer called for malformed request")
		return policystore.GroupOwnerTransfer{}, nil
	}}
	handler, err := HandlerWithConversations(Handler(nil), authFunc(verified), notCalled)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		``, `{}`, `null`, `[]`, `{"client_request_id":"bad"}`,
		strings.Replace(ownerTransferBody, `"target_interval_id"`, `"Target_interval_id"`, 1),
		strings.Replace(ownerTransferBody, `"target_interval_id"`, `"tenant_id":"`+tenantID+`","target_interval_id"`, 1),
		strings.TrimSuffix(ownerTransferBody, "}") + `,"source_interval_id":"` + groupIntervalID + `"}`,
		ownerTransferBody + ` true`, strings.Repeat(" ", 2048),
	} {
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, groupOwnerTransferRequest(body))
		if res.Code != http.StatusBadRequest {
			t.Fatalf("malformed transfer %q: %d %s", body[:min(len(body), 50)], res.Code, res.Body.String())
		}
	}
	wrongMethod := adminRequest(http.MethodGet, "/api/v1/groups/"+targetUserID+"/owner-transfers")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, wrongMethod)
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "POST" {
		t.Fatalf("wrong method: %d %s", res.Code, res.Body.String())
	}
	for _, failure := range []struct {
		err    error
		status int
		code   string
	}{
		{policystore.ErrInvalidGroupOwnerTransferRequest, 400, "invalid_request"},
		{policystore.ErrForbidden, 403, "invalid_identity"},
		{policystore.ErrGroupOwnerTransferPermissionDenied, 403, "group_permission_denied"},
		{policystore.ErrGroupNotAvailable, 404, "not_found"},
		{policystore.ErrGroupOwnerTransferConflict, 409, "idempotency_conflict"},
		{errors.Join(policystore.ErrAuditUnavailable, errors.New("private SQL detail")), 503, "unavailable"},
	} {
		failed := groupOwnerTransferStub{transfer: func(context.Context, access.TrustedIdentity, string, policystore.GroupOwnerTransferRequest) (policystore.GroupOwnerTransfer, error) {
			return policystore.GroupOwnerTransfer{}, failure.err
		}}
		h, _ := HandlerWithConversations(Handler(nil), authFunc(verified), failed)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, groupOwnerTransferRequest(ownerTransferBody))
		if res.Code != failure.status || !strings.Contains(res.Body.String(), failure.code) || strings.Contains(res.Body.String(), "private SQL detail") {
			t.Fatalf("error mapping: %d %s", res.Code, res.Body.String())
		}
	}
}
