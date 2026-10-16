package com.enterpriseim.client;

import android.content.*;
import android.database.Cursor;
import android.net.Uri;
import android.provider.DocumentsContract;
import java.io.IOException;
import java.util.UUID;
import java.util.function.BooleanSupplier;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

/** Application lifetime coordinator; all calls are serialized off the UI thread. */
public final class ExportManager {
    public static class CleanupException extends IOException {
        CleanupException(Exception cause) { super("Export cleanup pending", cause); }
    }
    public static final class ReauthorizationException extends CleanupException {
        public final ExportLedger.Reauthorization request;
        ReauthorizationException(ExportLedger.Reauthorization request, Exception cause) { super(cause); this.request = request; }
    }
    public static final class BusyException extends IOException {
        BusyException() { super("Another export is active"); }
    }
    private static ExportManager instance;
    public static synchronized ExportManager get(Context context) {
        if (instance == null) instance = new ExportManager(context.getApplicationContext());
        return instance;
    }
    public final ExecutorService worker = Executors.newSingleThreadExecutor();
    private final ContentResolver resolver;
    private final ExportLedger ledger;
    private String reservation;
    private Active active;
    private static final class Active {
        final String owner, ticket; final ExportSession session; boolean cleanupPending;
        Active(String owner, String ticket, ExportSession session) { this.owner = owner; this.ticket = ticket; this.session = session; }
    }
    private ExportManager(Context context) {
        resolver = context.getContentResolver();
        ledger = new ExportLedger(new ExportJournal(context.getNoBackupFilesDir()), new ExportLedger.Resources() {
            public int bootCount() {
                try { return android.provider.Settings.Global.getInt(resolver, android.provider.Settings.Global.BOOT_COUNT, -1); }
                catch (SecurityException unavailable) { return -1; }
            }
            public int grants(Uri uri) {
                int flags = 0;
                for (UriPermission grant : resolver.getPersistedUriPermissions()) if (grant.getUri().equals(uri)) {
                    if (grant.isReadPermission()) flags |= 1; if (grant.isWritePermission()) flags |= 2;
                }
                return flags;
            }
            public void take(Uri uri, int flags) { resolver.takePersistableUriPermission(uri, flags); }
            public void release(Uri uri, int flags) { resolver.releasePersistableUriPermission(uri, flags); }
            public boolean delete(Uri uri) throws Exception { return DocumentsContract.deleteDocument(resolver, uri); }
            public boolean absent(Uri uri) throws Exception {
                try (Cursor cursor = resolver.query(uri, new String[]{DocumentsContract.Document.COLUMN_DOCUMENT_ID}, null, null, null)) {
                    if (cursor == null) throw new IOException("Provider unavailable");
                    return cursor.getCount() == 0;
                }
            }
            public void validateTree(Uri tree) throws Exception {
                Uri parent = DocumentsContract.buildDocumentUriUsingTree(tree, DocumentsContract.getTreeDocumentId(tree));
                try (Cursor cursor = resolver.query(parent, new String[]{DocumentsContract.Document.COLUMN_MIME_TYPE, DocumentsContract.Document.COLUMN_FLAGS}, null, null, null)) {
                    if (cursor == null || !cursor.moveToFirst() ||
                        !DocumentsContract.Document.MIME_TYPE_DIR.equals(cursor.getString(0)) ||
                        (cursor.getInt(1) & DocumentsContract.Document.FLAG_DIR_SUPPORTS_CREATE) == 0) throw new IOException("Unsupported directory");
                }
            }
            public void create(Uri tree, String name) throws Exception {
                Uri parent = DocumentsContract.buildDocumentUriUsingTree(tree, DocumentsContract.getTreeDocumentId(tree));
                if (DocumentsContract.createDocument(resolver, parent, "application/octet-stream", name) == null) throw new IOException("Cannot allocate export");
            }
            public Uri findAllocated(Uri tree, String prefix) throws Exception {
                Uri children = DocumentsContract.buildChildDocumentsUriUsingTree(tree, DocumentsContract.getTreeDocumentId(tree));
                try (Cursor cursor = resolver.query(children, new String[]{DocumentsContract.Document.COLUMN_DOCUMENT_ID, DocumentsContract.Document.COLUMN_DISPLAY_NAME, DocumentsContract.Document.COLUMN_MIME_TYPE}, null, null, null)) {
                    return ExportDirectory.findAllocated(cursor, tree, prefix);
                }
            }
        });
    }
    public void reconcileStartup() {
        try { ledger.reconcileReleasedGrants(); }
        catch (Exception error) { android.util.Log.e("EnterpriseIM", "Export permission release reconciliation pending"); }
    }
    private void recover() throws CleanupException {
        try { ledger.recover(); } catch (Exception error) {
            ExportLedger.Reauthorization request = null;
            try { request = ledger.authorizationRequired(); } catch (Exception unreadable) { /* Invalid metadata stays blocked. */ }
            if (request != null) throw new ReauthorizationException(request, error);
            throw new CleanupException(error);
        }
    }
    public void reserve(String owner) throws Exception {
        if (active != null && active.cleanupPending) abort(active.owner, active.ticket);
        if (reservation != null || active != null) throw new BusyException();
        recover(); reservation = owner;
    }
    public void reauthorizeAndReserve(String owner, ExportLedger.Reauthorization request, Uri selected, int flags, BooleanSupplier ownerOpen) throws Exception {
        if (!ownerOpen.getAsBoolean()) throw new CleanupException(new IOException("Export owner closed"));
        if (reservation != null || active != null && !active.cleanupPending) throw new BusyException();
        try { ledger.reauthorize(request, selected, flags); } catch (Exception error) { throw new CleanupException(error); }
        if (!ownerOpen.getAsBoolean()) throw new CleanupException(new IOException("Export owner closed"));
        reserve(owner);
    }
    public void cancel(String owner) { if (owner.equals(reservation)) reservation = null; }
    public String accept(String owner, Uri tree, int flags, String name) throws Exception {
        if (!owner.equals(reservation)) throw new IllegalStateException("Export owner closed");
        reservation = null;
        String ticket = UUID.randomUUID().toString();
        Uri uri;
        try { uri = ledger.allocateTree(ticket, tree, name, flags); }
        catch (Exception error) { recover(); throw error; }
        active = new Active(owner, ticket, new ExportSession(
            () -> ProviderOutput.open(resolver, uri),
            () -> { recover(); return true; }));
        return ticket;
    }
    private Active require(String owner, String ticket) {
        if (active == null || !active.owner.equals(owner) || !active.ticket.equals(ticket)) throw new IllegalArgumentException("Invalid export ticket");
        return active;
    }
    public void write(String owner, String ticket, byte[] bytes) throws Exception {
        Active value = require(owner, ticket);
        try { value.session.write(bytes); } catch (Exception error) { abort(owner, ticket); throw error; }
    }
    public void finish(String owner, String ticket) throws Exception {
        Active value = require(owner, ticket);
        try { value.session.finish(); ledger.commit(ticket); }
        catch (Exception error) { abort(owner, ticket); throw error; }
        active = null;
        // A release failure retains COMMITTED; retry cannot delete the completed file.
        try { recover(); } catch (CleanupException error) { android.util.Log.e("EnterpriseIM", "Export URI release pending"); }
    }
    public void abort(String owner, String ticket) throws Exception {
        if (active == null || !active.owner.equals(owner) || !active.ticket.equals(ticket)) return;
        active.cleanupPending = true;
        active.session.abort(); active = null;
    }
    public void destroy(String owner) {
        cancel(owner);
        if (active == null || !active.owner.equals(owner)) return;
        // A failed abort already erased RAM. Retain its journal for an explicit retry;
        // destruction must not retry with access restored by an interrupted picker.
        if (active.cleanupPending) { active = null; return; }
        try { active.session.abort(); }
        catch (Exception error) { android.util.Log.e("EnterpriseIM", "Export destination cleanup failed during Activity destruction"); }
        finally { active = null; }
    }
}
