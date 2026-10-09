package policystore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/leileipei/Enterprise_IM/internal/access"
	"github.com/leileipei/Enterprise_IM/internal/policystore"
)

func deviceIdentity() access.TrustedIdentity {
	return access.TrustedIdentity{TenantID: tenantA, UserID: personA, ActingMembershipID: targetM}
}
func deviceOwner() policystore.DeviceOwner {
	return policystore.DeviceOwner{TenantID: tenantA, UserID: personA}
}
func deviceRegistration(n int) policystore.DeviceRegistration {
	return policystore.DeviceRegistration{RegistrationID: fmt.Sprintf("019a13c0-0000-7000-8000-%012d", n), Platform: "web", Name: "浏览器"}
}
func deviceService(c *pgx.Conn) policystore.Service {
	return policystore.Service{DB: c, Now: func() time.Time { return at }}
}

func TestRegisterDeviceConcurrentQuotaAndResponseLoss(t *testing.T) {
	c := db(t)
	seed(t, c)
	ctx := context.Background()
	const count = 14
	peers := make([]*pgx.Conn, count)
	for i := range peers {
		peers[i] = filePeer(t, c)
	}
	type outcome struct {
		grant policystore.DeviceGrant
		err   error
	}
	results := make(chan outcome, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func(i int, p *pgx.Conn) {
			defer wg.Done()
			<-start
			s := deviceService(p)
			s.DeviceRegistrationsPerHour = 20
			g, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(i+1))
			results <- outcome{g, e}
		}(i, p)
	}
	close(start)
	wg.Wait()
	close(results)
	grants := []policystore.DeviceGrant{}
	for r := range results {
		if r.err == nil {
			grants = append(grants, r.grant)
		} else if !errors.Is(r.err, policystore.ErrDeviceQuotaExceeded) {
			t.Fatal(r.err)
		}
	}
	if len(grants) != 10 {
		t.Fatalf("concurrent quota: %d", len(grants))
	}
	s := deviceService(c)
	s.DeviceRegistrationsPerHour = 20
	var registration string
	if err := c.QueryRow(ctx, "SELECT registration_id::text FROM user_devices WHERE id=$1", grants[0].DeviceID).Scan(&registration); err != nil {
		t.Fatal(err)
	}
	req := deviceRegistration(1)
	req.RegistrationID = registration
	_, e := s.RegisterDevice(ctx, deviceIdentity(), req)
	var duplicate *policystore.DeviceRegistrationExistsError
	if !errors.As(e, &duplicate) || duplicate.DeviceID != grants[0].DeviceID {
		t.Fatalf("duplicate ownership: %v", e)
	}
	var used int
	if err := c.QueryRow(ctx, "SELECT created_count FROM device_registration_windows WHERE tenant_id=$1 AND user_id=$2", tenantA, personA).Scan(&used); err != nil || used != 10 {
		t.Fatalf("window count: %d %v", used, err)
	}
	for _, g := range grants {
		raw, e := base64.RawURLEncoding.Strict().DecodeString(g.Token)
		if e != nil || len(raw) != 32 || len(g.Token) != 43 {
			t.Fatal("bad secret shape")
		}
		digest := sha256.Sum256(raw)
		var stored []byte
		if e = c.QueryRow(ctx, "SELECT credential_digest FROM user_devices WHERE id=$1", g.DeviceID).Scan(&stored); e != nil || !bytes.Equal(stored, digest[:]) || bytes.Equal(stored, raw) {
			t.Fatal("only SHA256 digest must be stored")
		}
		if !g.ExpiresAt.Equal(at.Add(30*24*time.Hour)) || !g.ServerTime.Equal(at) {
			t.Fatal("lease boundary")
		}
	}
	// The first response may have been lost. The owner can revoke the orphan;
	// a new registration is allowed only when the hourly budget also permits it.
	if e = s.RevokeDevice(ctx, deviceOwner(), grants[0].DeviceID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(99)); e != nil {
		t.Fatal(e)
	}
}

func TestRegisterDeviceHourlyWindowDoesNotResetOnRevokeOrDuplicate(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	var last policystore.DeviceGrant
	for n := 1; n <= 10; n++ {
		g, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(n))
		if e != nil {
			t.Fatal(e)
		}
		last = g
		if e = s.RevokeDevice(ctx, deviceOwner(), g.DeviceID); e != nil {
			t.Fatal(e)
		}
	}
	_, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(10))
	var dup *policystore.DeviceRegistrationExistsError
	if !errors.As(e, &dup) || dup.DeviceID != last.DeviceID {
		t.Fatal("duplicate must not spend hourly budget")
	}
	if _, e = s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(11)); !errors.Is(e, policystore.ErrDeviceRegistrationRateLimited) {
		t.Fatalf("hourly quota: %v", e)
	}
	s.Now = func() time.Time { return at.Add(time.Hour) }
	if _, e = s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(11)); e != nil {
		t.Fatal(e)
	}
	var used int
	if e = c.QueryRow(ctx, "SELECT created_count FROM device_registration_windows WHERE tenant_id=$1 AND user_id=$2", tenantA, personA).Scan(&used); e != nil || used != 1 {
		t.Fatalf("window rollover %d %v", used, e)
	}
}

func TestDeviceOwnerWithoutActingMembershipCanRevoke(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	g, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	run(t, c, "UPDATE user_organizations SET status='ended',effective_to=$3 WHERE tenant_id=$1 AND user_id=$2", tenantA, personA, at)
	p, e := s.ListDevices(ctx, deviceOwner(), "", 20)
	if e != nil || len(p.Devices) != 1 {
		t.Fatalf("account-only list: %v", e)
	}
	if e = s.RevokeDevice(ctx, deviceOwner(), g.DeviceID); e != nil {
		t.Fatal(e)
	}
	if e = s.RevokeDevice(ctx, deviceOwner(), g.DeviceID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(2)); !errors.Is(e, policystore.ErrForbidden) {
		t.Fatalf("ended identity: %v", e)
	}
	var count int
	if e = c.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE actor_user_id=$1 AND action IN ('device_list','device_revoke') AND acting_membership_id IS NULL", personA).Scan(&count); e != nil || count != 3 {
		t.Fatalf("owner audits %d %v", count, e)
	}
	for _, owner := range []policystore.DeviceOwner{{TenantID: tenantA, UserID: adminA}, {TenantID: tenantB, UserID: personB}} {
		if e = s.RevokeDevice(ctx, owner, g.DeviceID); !errors.Is(e, policystore.ErrDeviceNotFound) {
			t.Fatal("foreign device must be hidden", e)
		}
	}
	run(t, c, "UPDATE users SET status='frozen' WHERE id=$1", personA)
	if _, e = s.ListDevices(ctx, deviceOwner(), "", 20); !errors.Is(e, policystore.ErrForbidden) {
		t.Fatal("inactive account list", e)
	}
}

func TestDeviceRevocationBlocksRenewal(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	g, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	cred := policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}
	s.Now = func() time.Time { return at.Add(24 * time.Hour) }
	renewed, e := s.RenewDevice(ctx, deviceIdentity(), cred)
	if e != nil || renewed.Token != "" || !renewed.ExpiresAt.Equal(at.Add(31*24*time.Hour)) {
		t.Fatalf("renew must not return token: %v", e)
	}
	for _, bad := range []policystore.DeviceCredential{{ID: g.DeviceID, Token: g.Token + "="}, {ID: g.DeviceID, Token: strings.Repeat("A", 43)}, {ID: adminA, Token: g.Token}} {
		if _, e = s.RenewDevice(ctx, deviceIdentity(), bad); !errors.Is(e, policystore.ErrInvalidDevice) {
			t.Fatal("invalid device", e)
		}
	}
	if _, e = s.RenewDevice(ctx, publisher(), cred); !errors.Is(e, policystore.ErrInvalidDevice) {
		t.Fatal("foreign owner credential", e)
	}
	if e = s.RevokeDevice(ctx, deviceOwner(), g.DeviceID); e != nil {
		t.Fatal(e)
	}
	if _, e = s.RenewDevice(ctx, deviceIdentity(), cred); !errors.Is(e, policystore.ErrInvalidDevice) {
		t.Fatal("revoked renewal", e)
	}
	expired, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(2))
	if e != nil {
		t.Fatal(e)
	}
	s.Now = func() time.Time { return expired.ExpiresAt }
	if _, e = s.RenewDevice(ctx, deviceIdentity(), policystore.DeviceCredential{ID: expired.DeviceID, Token: expired.Token}); !errors.Is(e, policystore.ErrInvalidDevice) {
		t.Fatal("expiry equality must reject", e)
	}
}

func TestDeviceAuditFailureRollsBack(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	g, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	run(t, c, `CREATE FUNCTION fail_device_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$; CREATE TRIGGER fail_device_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION fail_device_audit()`)
	if _, e = s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(2)); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal(e)
	}
	if e = s.RevokeDevice(ctx, deviceOwner(), g.DeviceID); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal(e)
	}
	s.Now = func() time.Time { return at.Add(time.Hour) }
	if _, e = s.RenewDevice(ctx, deviceIdentity(), policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}); !errors.Is(e, policystore.ErrAuditUnavailable) {
		t.Fatal(e)
	}
	var count, used int
	var revoked *time.Time
	var expiry time.Time
	if e = c.QueryRow(ctx, "SELECT count(*) FROM user_devices").Scan(&count); e != nil || count != 1 {
		t.Fatal("registration leaked")
	}
	if e = c.QueryRow(ctx, "SELECT revoked_at,expires_at FROM user_devices WHERE id=$1", g.DeviceID).Scan(&revoked, &expiry); e != nil || revoked != nil || !expiry.Equal(g.ExpiresAt) {
		t.Fatal("revoke/renew leaked")
	}
	if e = c.QueryRow(ctx, "SELECT created_count FROM device_registration_windows WHERE tenant_id=$1 AND user_id=$2", tenantA, personA).Scan(&used); e != nil || used != 1 {
		t.Fatal("window leaked")
	}
}

func TestDeviceOwnerPaginationAndInvalidInputs(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx := context.Background()
	for n := 1; n <= 3; n++ {
		r := deviceRegistration(n)
		r.Platform = []string{"windows", "android", "ios"}[n-1]
		if _, e := s.RegisterDevice(ctx, deviceIdentity(), r); e != nil {
			t.Fatal(e)
		}
	}
	first, e := s.ListDevices(ctx, deviceOwner(), "", 2)
	if e != nil || len(first.Devices) != 2 || !first.HasMore || first.NextCursor == "" {
		t.Fatal("first page", e)
	}
	next, e := s.ListDevices(ctx, deviceOwner(), first.NextCursor, 2)
	if e != nil || len(next.Devices) != 1 || next.HasMore {
		t.Fatal("next page", e)
	}
	if next.Devices[0].ID == first.Devices[0].ID || next.Devices[0].ID == first.Devices[1].ID {
		t.Fatal("duplicate page")
	}
	for _, r := range []policystore.DeviceRegistration{{RegistrationID: adminA, Platform: "web", Name: "browser"}, {RegistrationID: deviceRegistration(4).RegistrationID, Platform: "linux", Name: "browser"}, {RegistrationID: deviceRegistration(4).RegistrationID, Platform: "web", Name: " x"}, {RegistrationID: deviceRegistration(4).RegistrationID, Platform: "web", Name: "x\n"}, {RegistrationID: deviceRegistration(4).RegistrationID, Platform: "web", Name: strings.Repeat("中", 81)}} {
		if _, e = s.RegisterDevice(ctx, deviceIdentity(), r); !errors.Is(e, policystore.ErrInvalidDeviceRequest) {
			t.Fatal("invalid registration", e)
		}
	}
	if _, e = s.ListDevices(ctx, deviceOwner(), "garbage", 20); !errors.Is(e, policystore.ErrInvalidDeviceRequest) {
		t.Fatal(e)
	}
	s.Now = func() time.Time { return at.Add(30 * 24 * time.Hour) }
	expired, e := s.ListDevices(ctx, deviceOwner(), "", 20)
	if e != nil || len(expired.Devices) != 3 {
		t.Fatal(e)
	}
	for _, d := range expired.Devices {
		if d.Status != "expired" {
			t.Fatal("expiry status")
		}
	}
}

type afterDeviceAuditDB struct {
	access.Beginner
	after func()
}

func (d afterDeviceAuditDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, e := d.Beginner.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return afterDeviceAuditTx{Tx: tx, after: d.after}, nil
}

type afterDeviceAuditTx struct {
	pgx.Tx
	after func()
}

func (tx afterDeviceAuditTx) Exec(ctx context.Context, q string, args ...any) (pgconn.CommandTag, error) {
	tag, e := tx.Tx.Exec(ctx, q, args...)
	if e == nil && strings.Contains(q, "INSERT INTO audit_events") {
		tx.after()
	}
	return tag, e
}

func TestDeviceRevocationSerializedWithRenewal(t *testing.T) {
	c := db(t)
	seed(t, c)
	s := deviceService(c)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(1))
	if e != nil {
		t.Fatal(e)
	}
	blocker, e := c.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer blocker.Rollback(ctx)
	if _, e = blocker.Exec(ctx, "UPDATE user_devices SET revoked_at=$2 WHERE id=$1", g.DeviceID, at); e != nil {
		t.Fatal(e)
	}
	peer := filePeer(t, c)
	done := make(chan error, 1)
	go func() {
		_, e := deviceService(peer).RenewDevice(ctx, deviceIdentity(), policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token})
		done <- e
	}()
	// Observer needs a separate connection while c owns the revocation tx.
	observer := filePeer(t, c)
	waitFileSendLock(t, observer, peer.PgConn().PID())
	if e = blocker.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-done:
		if !errors.Is(e, policystore.ErrInvalidDevice) {
			t.Fatal("committed revocation bypassed", e)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestDeviceFinalAuthorizationAfterAudit(t *testing.T) {
	for _, operation := range []string{"register", "renew"} {
		t.Run(operation, func(t *testing.T) {
			c := db(t)
			seed(t, c)
			s := deviceService(c)
			ctx := context.Background()
			clock := at
			if operation == "register" {
				run(t, c, "UPDATE user_organizations SET effective_to=$2 WHERE id=$1", targetM, at.Add(time.Second))
				s.DB = afterDeviceAuditDB{Beginner: c, after: func() { clock = at.Add(time.Second) }}
				s.Now = func() time.Time { return clock }
				if _, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(1)); !errors.Is(e, policystore.ErrForbidden) {
					t.Fatal("late membership expiry", e)
				}
				var n int
				if e := c.QueryRow(ctx, "SELECT count(*) FROM user_devices").Scan(&n); e != nil || n != 0 {
					t.Fatal("late registration committed")
				}
			} else {
				g, e := s.RegisterDevice(ctx, deviceIdentity(), deviceRegistration(1))
				if e != nil {
					t.Fatal(e)
				}
				s.DB = afterDeviceAuditDB{Beginner: c, after: func() { clock = g.ExpiresAt }}
				s.Now = func() time.Time { return clock }
				if _, e = s.RenewDevice(ctx, deviceIdentity(), policystore.DeviceCredential{ID: g.DeviceID, Token: g.Token}); !errors.Is(e, policystore.ErrInvalidDevice) {
					t.Fatal("late lease expiry", e)
				}
				var seen *time.Time
				if e = c.QueryRow(ctx, "SELECT last_seen_at FROM user_devices WHERE id=$1", g.DeviceID).Scan(&seen); e != nil || seen != nil {
					t.Fatal("late renewal committed")
				}
			}
		})
	}
}
