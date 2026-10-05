package policystore_test

import (
	"context"
	"testing"
)

func TestWebFileRealContextIsolation(t *testing.T) {
	t.Run("actual false then true is awaited", webFileAwaitCondition)
	f := newWebFileFixture(t)
	samples := f.samples(t, false)
	m := f.uploadReady(t, directA, "上下文私有报告.txt", []byte("context bytes"))
	f.sendFile(t, directA, "direct", m.ID, "")
	f.installContextStream(t)
	f.browser(t, "file_context", map[string]any{"conversation": directA, "fileID": m.ID, "name": m.OriginalFilename, "sample": samples[0]})
	f.assertPrivate(t)
}
func TestWebFileRealUnknownUploadSend(t *testing.T) {
	f := newWebFileFixture(t)
	samples := f.samples(t, false)
	f.browser(t, "file_lifecycle", map[string]any{"conversation": directA, "kind": "direct", "unknown": true, "samples": samples[:1]})
	for _, table := range []string{"file_objects", "messages", "message_attachments", "outbox_events"} {
		var n int
		if e := f.real.conn.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); e != nil || n != 1 {
			t.Fatal("unknown response caused duplicate", table, n, e)
		}
	}
	f.assertPrivate(t)
	t.Run("ready recovers after query failure", webFileStatusRecovery)
	t.Run("frozen send survives overlapping query failure", webFileStatusOverlap)
	t.Run("ACK stays saved after failed pull", webFileACKPullFailure)
	t.Run("group pending survives refresh", webFileGroupComposer)
	t.Run("group saved survives refresh", webFileGroupSavedRefresh)
}
func TestWebFileRealDownloadFaults(t *testing.T) {
	f := newWebFileFixture(t)
	m := f.uploadReady(t, directA, "断流私有报告.txt", []byte("controlled real stream bytes"))
	f.sendFile(t, directA, "direct", m.ID, "")
	f.installDownloadFaults(t)
	f.browser(t, "file_download_faults", map[string]any{"conversation": directA, "name": m.OriginalFilename})
	f.assertPrivate(t)
}
func TestWebFileRealRevocation(t *testing.T) {
	requireWebFiles(t)
	t.Run("actual policy requester uploader TTL revocation", TestFileDownloadRealRevocation)
	t.Run("actual TCP token expiry", TestFileDownloadRealTokenExpiryBlockedWrite)
	t.Run("actual TCP scheduled policy", TestFileDownloadRealTCPRevocation)
	t.Run("pending deleted and source guards", TestFileDownloadAuthorizationAdminNoBypass)
	t.Run("dynamic existing retention", TestFileDownloadAuthorizationDynamicRetention)
}
func TestWebFileRealPolicyConflict(t *testing.T) {
	f := newWebFileFixture(t)
	run(t, f.real.conn, "UPDATE tenant_file_upload_policy SET version=9007199254740993 WHERE tenant_id=$1", tenantA)
	f.installPolicyConflict(t)
	f.browser(t, "file_settings", map[string]any{"conflict": true})
	var version int64
	if e := f.real.conn.QueryRow(context.Background(), "SELECT version FROM tenant_file_upload_policy WHERE tenant_id=$1", tenantA).Scan(&version); e != nil || version != 9007199254740995 {
		t.Fatal("exact CAS/readback version mismatch", version, e)
	}
	f.assertPrivate(t)
}

func webFileACKPullFailure(t *testing.T) {
	f := newWebFileFixture(t)
	samples := f.samples(t, false)
	f.browser(t, "file_lifecycle", map[string]any{"conversation": directA, "kind": "direct", "ackPullFault": true, "samples": samples[:1]})
	f.assertPrivate(t)
}

func webFileGroupComposer(t *testing.T) {
	f := newWebFileFixture(t)
	samples := f.samples(t, false)
	f.browser(t, "file_lifecycle", map[string]any{"conversation": f.group(t), "kind": "group", "unknown": true, "groupComposer": true, "samples": samples[:1]})
	f.assertPrivate(t)
}

func webFileGroupSavedRefresh(t *testing.T) {
	f := newWebFileFixture(t)
	samples := f.samples(t, false)
	f.browser(t, "file_lifecycle", map[string]any{"conversation": f.group(t), "kind": "group", "groupSavedRefresh": true, "samples": samples[:1]})
	f.assertPrivate(t)
}

func webFileStatusRecovery(t *testing.T) { webFileStatusScenario(t, false) }
func webFileStatusOverlap(t *testing.T)  { webFileStatusScenario(t, true) }
func webFileStatusScenario(t *testing.T, overlap bool) {
	f := newWebFileFixture(t)
	samples := f.samples(t, false)
	samples[0].SavedName = "_" + samples[0].Name // Chrome substitutes underscore for a leading FEFF in local names.
	samples[0].Name = "\uFEFF" + samples[0].Name
	f.private = append(f.private, samples[0].Name)
	data := map[string]any{"conversation": directA, "kind": "direct", "samples": samples[:1]}
	if overlap {
		data["unknown"] = true
		data["statusOverlap"] = true
	} else {
		data["statusRecovery"] = true
	}
	f.browser(t, "file_lifecycle", data)
	for _, table := range []string{"file_objects", "messages", "message_attachments", "outbox_events"} {
		var count int
		if e := f.real.conn.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count); e != nil || count != 1 {
			t.Fatal("status recovery lost or duplicated attachment", table, count, e)
		}
	}
	var kind string
	if e := f.real.conn.QueryRow(context.Background(), "SELECT message_type FROM messages").Scan(&kind); e != nil || kind != "file" {
		t.Fatal("status recovery sent plain text", kind, e)
	}
	f.assertPrivate(t)
}
