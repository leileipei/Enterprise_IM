package com.enterpriseim.client;

import android.net.Uri;
import java.io.IOException;

/** Recovery state machine. A committed file is never deleted by recovery. */
public final class ExportLedger {
    public interface Resources {
        int grants(Uri uri);
        default int bootCount() { return -1; }
        void take(Uri uri, int flags) throws Exception;
        void release(Uri uri, int flags) throws Exception;
        boolean delete(Uri uri) throws Exception;
        boolean absent(Uri uri) throws Exception;
        default void validateTree(Uri tree) throws Exception { throw new IOException("Unsupported directory"); }
        default void create(Uri tree, String name) throws Exception { throw new IOException("Unsupported directory"); }
        default Uri findAllocated(Uri tree, String prefix) throws Exception { throw new IOException("Unsupported directory"); }
    }
    private final ExportJournal journal;
    private final Resources resources;
    private final ExportGrantReleases releases;
    public ExportLedger(ExportJournal journal, Resources resources) { this.journal = journal; this.resources = resources; this.releases = new ExportGrantReleases(journal.directory(), resources); }
    public void prepare(String ticket, Uri uri, int offered) throws Exception {
        if (journal.read() != null) throw new IOException("Unresolved export");
        int wanted = offered & 3;
        int acquired = wanted & ~resources.grants(uri);
        releases.ensureCapacity(uri, acquired);
        ExportJournal.Entry entry = new ExportJournal.Entry(ticket, uri, acquired, "PREPARED");
        journal.write(entry);
        if ((wanted & 2) == 0 || (offered & 64) == 0) throw new IOException("Durable URI access unavailable");
        releases.acquiring(uri, acquired);
        resources.take(uri, wanted);
        if ((resources.grants(uri) & wanted) != wanted) throw new IOException("Durable URI access not confirmed");
        journal.write(entry.phase("PENDING"));
    }
    /** Persist access and an unpredictable ownership prefix before the provider can create a file. */
    public Uri allocateTree(String ticket, Uri tree, String requested, int offered) throws Exception {
        if (journal.read() != null) throw new IOException("Unresolved export");
        if (!android.provider.DocumentsContract.isTreeUri(tree)) throw new IOException("Directory required");
        String prefix = "im-" + ticket + "-";
        String name = allocatedName(prefix, requested);
        int wanted = offered & 3, acquired = wanted & ~resources.grants(tree);
        releases.ensureCapacity(tree, acquired);
        ExportJournal.Entry entry = new ExportJournal.Entry(ticket, Uri.EMPTY, tree, acquired, "TREE_PREPARED", prefix, name);
        journal.write(entry);
        if (wanted != 3 || (offered & 64) == 0) throw new IOException("Durable directory access unavailable");
        releases.acquiring(tree, acquired);
        resources.take(tree, wanted);
        if ((resources.grants(tree) & wanted) != wanted) throw new IOException("Durable directory access not confirmed");
        resources.validateTree(tree);
        if (resources.findAllocated(tree, prefix) != null) throw new IOException("Allocation prefix already exists");
        journal.write(entry.phase("ALLOCATING"));
        resources.create(tree, name);
        Uri document = resources.findAllocated(tree, prefix);
        if (document == null) throw new IOException("Allocated document not visible");
        journal.write(entry.document(document));
        return document;
    }
    private static String allocatedName(String prefix, String requested) throws IOException {
        if (requested == null || requested.isEmpty() || !requested.equals(requested.trim()) ||
            requested.equals(".") || requested.equals("..") || requested.getBytes(java.nio.charset.StandardCharsets.UTF_8).length > 255)
            throw new IOException("Invalid filename");
        boolean visible = false;
        for (int i = 0; i < requested.length(); i++) {
            char c = requested.charAt(i);
            if (Character.isISOControl(c) || c == '/' || c == '\\' || c == ':') throw new IOException("Invalid filename");
            if (!Character.isWhitespace(c)) visible = true;
        }
        if (!visible) throw new IOException("Invalid filename");
        int dot = requested.lastIndexOf('.');
        String extension = dot > 0 ? requested.substring(dot) : "";
        if (extension.getBytes(java.nio.charset.StandardCharsets.UTF_8).length > 32) extension = "";
        String stem = extension.isEmpty() ? requested : requested.substring(0, dot);
        int budget = 255 - prefix.getBytes(java.nio.charset.StandardCharsets.UTF_8).length - extension.getBytes(java.nio.charset.StandardCharsets.UTF_8).length;
        int end = 0;
        while (end < stem.length()) {
            int next = end + Character.charCount(stem.codePointAt(end));
            if (stem.substring(0, next).getBytes(java.nio.charset.StandardCharsets.UTF_8).length > budget) break;
            end = next;
        }
        if (end == 0) throw new IOException("Invalid filename");
        return prefix + stem.substring(0, end) + extension;
    }
    /** Only an unfinished, validated tree operation can request renewed access. */
    public static final class Reauthorization {
        public final String ticket;
        public final Uri tree;
        private Reauthorization(String ticket, Uri tree) { this.ticket = ticket; this.tree = tree; }
    }
    public Reauthorization authorizationRequired() throws Exception {
        ExportJournal.Entry entry = journal.read();
        if (entry == null || !entry.treeBased() ||
            !(entry.phase.equals("ALLOCATING") || entry.phase.equals("PENDING"))) return null;
        return (resources.grants(entry.grantUri) & 3) == 3 ? null : new Reauthorization(entry.ticket, entry.grantUri);
    }
    /** Selection only restores the original operation; it never creates or deletes a document. */
    public void reauthorize(Reauthorization request, Uri selected, int offered) throws Exception {
        ExportJournal.Entry entry = journal.read();
        if (request == null || entry == null || !entry.treeBased() ||
            !entry.ticket.equals(request.ticket) || !entry.grantUri.equals(request.tree) || !entry.grantUri.equals(selected) ||
            !(entry.phase.equals("ALLOCATING") || entry.phase.equals("PENDING")) ||
            (offered & 3) != 3 || (offered & 64) == 0) throw new IOException("Original export directory required");
        int newlyAcquired = 3 & ~resources.grants(entry.grantUri);
        int acquired = entry.acquired | newlyAcquired;
        releases.ensureCapacity(entry.grantUri, acquired);
        // Record any newly owned bits before taking them, including a partial take failure.
        journal.write(new ExportJournal.Entry(entry.ticket, entry.uri, entry.grantUri, acquired, entry.phase, entry.prefix, entry.displayName));
        releases.acquiring(entry.grantUri, acquired);
        resources.take(entry.grantUri, 3);
        if ((resources.grants(entry.grantUri) & 3) != 3) throw new IOException("Durable directory access not confirmed");
    }
    public void commit(String ticket) throws Exception {
        ExportJournal.Entry entry = require(ticket);
        journal.write(entry.phase("COMMITTED"));
    }
    public void recover() throws Exception {
        ExportJournal.Entry entry = journal.read(); if (entry == null) { releases.reconcile(null); return; }
        if (entry.phase.equals("TREE_PREPARED")) {
            // No create call can be issued until ALLOCATING has been durably saved.
            entry = entry.phase("DELETED"); journal.write(entry);
        }
        if (entry.phase.equals("ALLOCATING")) {
            Uri document = resources.findAllocated(entry.grantUri, entry.prefix);
            // Empty is ambiguous while a provider may still be creating after caller death.
            if (document == null) throw new IOException("Allocation outcome unresolved");
            entry = entry.document(document); journal.write(entry);
        }
        if (entry.phase.equals("PREPARED") || entry.phase.equals("PENDING")) {
            if (entry.treeBased()) {
                Uri observed = resources.findAllocated(entry.grantUri, entry.prefix);
                if (observed != null && !observed.equals(entry.uri)) throw new IOException("Allocation identity mismatch");
                if (observed == null) {
                    // A known allocated document is now absent from the authoritative directory listing.
                    entry = entry.phase("DELETED"); journal.write(entry);
                }
            }
            // Only an authoritative empty query proves a prior successful deletion.
            // An unavailable provider, null cursor or ambiguous exception stays blocked.
            if (!entry.phase.equals("DELETED")) {
                boolean deleted;
                try { deleted = resources.delete(entry.uri); }
                catch (Exception error) { if (!absent(entry)) throw error; deleted = true; }
                if (!deleted && !absent(entry)) throw new IOException("Export destination cleanup failed");
                entry = entry.phase("DELETED"); journal.write(entry);
            }
        }
        releases.release(entry.grantUri, entry.acquired);
        journal.clear();
        releases.reconcile(null);
    }
    /** Startup never deletes files and must preserve access needed by a durable unfinished operation. */
    public void reconcileReleasedGrants() throws Exception {
        ExportJournal.Entry entry = journal.read();
        boolean pending = entry != null && !(entry.phase.equals("COMMITTED") || entry.phase.equals("DELETED"));
        releases.reconcile(pending ? entry.grantUri : null);
    }

    private boolean absent(ExportJournal.Entry entry) throws Exception {
        if (!entry.treeBased()) return resources.absent(entry.uri);
        return resources.findAllocated(entry.grantUri, entry.prefix) == null;
    }
    private ExportJournal.Entry require(String ticket) throws Exception {
        ExportJournal.Entry entry = journal.read();
        if (entry == null || !entry.ticket.equals(ticket) || !entry.phase.equals("PENDING")) throw new IOException("Invalid export ticket");
        return entry;
    }
}
