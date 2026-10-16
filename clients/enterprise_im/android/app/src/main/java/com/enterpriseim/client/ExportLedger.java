package com.enterpriseim.client;

import android.net.Uri;
import java.io.IOException;

/** Recovery state machine. A committed file is never deleted by recovery. */
public final class ExportLedger {
    public interface Resources {
        int grants(Uri uri);
        void take(Uri uri, int flags) throws Exception;
        void release(Uri uri, int flags) throws Exception;
        boolean delete(Uri uri) throws Exception;
        boolean absent(Uri uri) throws Exception;
    }
    private final ExportJournal journal;
    private final Resources resources;
    public ExportLedger(ExportJournal journal, Resources resources) { this.journal = journal; this.resources = resources; }
    public void prepare(String ticket, Uri uri, int offered) throws Exception {
        if (journal.read() != null) throw new IOException("Unresolved export");
        int wanted = offered & 3;
        int acquired = wanted & ~resources.grants(uri);
        ExportJournal.Entry entry = new ExportJournal.Entry(ticket, uri, acquired, "PREPARED");
        journal.write(entry);
        if ((wanted & 2) == 0 || (offered & 64) == 0) throw new IOException("Durable URI access unavailable");
        resources.take(uri, wanted);
        if ((resources.grants(uri) & wanted) != wanted) throw new IOException("Durable URI access not confirmed");
        journal.write(entry.phase("PENDING"));
    }
    public void commit(String ticket) throws Exception {
        ExportJournal.Entry entry = require(ticket);
        journal.write(entry.phase("COMMITTED"));
    }
    public void recover() throws Exception {
        ExportJournal.Entry entry = journal.read(); if (entry == null) return;
        if (entry.phase.equals("PREPARED") || entry.phase.equals("PENDING")) {
            // Only an authoritative empty query proves a prior successful deletion.
            // An unavailable provider, null cursor or ambiguous exception stays blocked.
            boolean deleted;
            try { deleted = resources.delete(entry.uri); }
            catch (Exception error) { if (!resources.absent(entry.uri)) throw error; deleted = true; }
            if (!deleted && !resources.absent(entry.uri)) throw new IOException("Export destination cleanup failed");
            entry = entry.phase("DELETED"); journal.write(entry);
        }
        int held = resources.grants(entry.uri) & entry.acquired;
        if (held != 0) resources.release(entry.uri, held);
        if ((resources.grants(entry.uri) & entry.acquired) != 0) throw new IOException("URI release not confirmed");
        journal.clear();
    }
    private ExportJournal.Entry require(String ticket) throws Exception {
        ExportJournal.Entry entry = journal.read();
        if (entry == null || !entry.ticket.equals(ticket) || !entry.phase.equals("PENDING")) throw new IOException("Invalid export ticket");
        return entry;
    }
}
