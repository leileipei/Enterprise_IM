package com.enterpriseim.client;

import android.net.Uri;
import android.util.AtomicFile;
import org.json.JSONObject;
import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.UUID;

/** Only ownership metadata, never message/file contents. Fail closed on damaged metadata. */
public final class ExportJournal {
    public static final class Entry {
        public final String ticket, phase, prefix, displayName;
        public final Uri uri, grantUri;
        public final int acquired;
        public Entry(String ticket, Uri uri, int acquired, String phase) {
            this(ticket, uri, uri, acquired, phase, null, null);
        }
        public Entry(String ticket, Uri uri, Uri grantUri, int acquired, String phase, String prefix, String displayName) {
            this.ticket = ticket; this.uri = uri; this.grantUri = grantUri; this.acquired = acquired;
            this.phase = phase; this.prefix = prefix; this.displayName = displayName;
        }
        public boolean treeBased() { return prefix != null; }
        Entry phase(String next) { return new Entry(ticket, uri, grantUri, acquired, next, prefix, displayName); }
        Entry document(Uri document) { return new Entry(ticket, document, grantUri, acquired, "PENDING", prefix, displayName); }
    }
    private final AtomicFile file;
    public ExportJournal(File directory) { file = new AtomicFile(new File(directory, "export-cleanup.json")); }
    public Entry read() throws Exception {
        File base = file.getBaseFile();
        if (!base.exists() && !new File(base + ".bak").exists()) {
            if (new File(base + ".new").exists()) throw new IOException("Incomplete export metadata");
            return null;
        }
        byte[] bytes;
        try (InputStream in = file.openRead(); ByteArrayOutputStream out = new ByteArrayOutputStream()) {
            byte[] block = new byte[1024]; int n;
            while ((n = in.read(block)) != -1) {
                if (out.size() + n > 16384) throw new IOException("Oversized export metadata");
                out.write(block, 0, n);
            }
            bytes = out.toByteArray();
        }
        JSONObject json = new JSONObject(new String(bytes, StandardCharsets.UTF_8));
        if (!(json.get("version") instanceof Integer) || !(json.get("acquired") instanceof Integer)) throw new IOException("Invalid export metadata");
        int version = json.getInt("version"), flags = json.getInt("acquired");
        String ticket = json.getString("ticket"), phase = json.getString("phase");
        Uri uri = Uri.parse(json.getString("uri"));
        if (!UUID.fromString(ticket).toString().equals(ticket) || flags < 0 || (flags & ~3) != 0) throw new IOException("Invalid export metadata");
        if (version == 1 && json.length() == 5 && content(uri) &&
            (phase.equals("PREPARED") || phase.equals("PENDING") || phase.equals("COMMITTED") || phase.equals("DELETED")))
            return new Entry(ticket, uri, flags, phase);
        if (version != 2 || json.length() != 8) throw new IOException("Unknown export metadata");
        Uri grant = Uri.parse(json.getString("grant_uri"));
        String prefix = json.getString("prefix"), name = json.getString("display_name");
        boolean empty = uri.equals(Uri.EMPTY);
        if (!content(grant) || !android.provider.DocumentsContract.isTreeUri(grant) ||
            !prefix.equals("im-" + ticket + "-") || !name.startsWith(prefix) || name.getBytes(StandardCharsets.UTF_8).length > 255 ||
            name.indexOf('/') >= 0 || name.indexOf('\\') >= 0 || name.indexOf(':') >= 0 ||
            !((phase.equals("TREE_PREPARED") || phase.equals("ALLOCATING")) && empty ||
              phase.equals("DELETED") && (empty || content(uri)) ||
              (phase.equals("PENDING") || phase.equals("COMMITTED")) && content(uri)) ||
            !empty && !within(grant, uri)) throw new IOException("Invalid tree export metadata");
        return new Entry(ticket, uri, grant, flags, phase, prefix, name);
    }
    private static boolean within(Uri grant, Uri document) {
        try {
            return grant.getAuthority().equals(document.getAuthority()) &&
                android.provider.DocumentsContract.isTreeUri(document) &&
                android.provider.DocumentsContract.getTreeDocumentId(grant).equals(android.provider.DocumentsContract.getTreeDocumentId(document)) &&
                !android.provider.DocumentsContract.getDocumentId(document).isEmpty();
        } catch (Exception invalid) { return false; }
    }
    private static boolean content(Uri uri) { return "content".equals(uri.getScheme()) && uri.getAuthority() != null; }
    public void write(Entry entry) throws Exception {
        JSONObject json = new JSONObject();
        json.put("version", entry.treeBased() ? 2 : 1).put("ticket", entry.ticket).put("uri", entry.uri.toString())
            .put("acquired", entry.acquired).put("phase", entry.phase);
        if (entry.treeBased()) json.put("grant_uri", entry.grantUri.toString()).put("prefix", entry.prefix).put("display_name", entry.displayName);
        byte[] bytes = json.toString().getBytes(StandardCharsets.UTF_8);
        if (bytes.length > 16384) throw new IOException("Oversized export metadata");
        FileOutputStream stream = file.startWrite();
        try { stream.write(bytes); stream.getFD().sync(); file.finishWrite(stream); }
        catch (Exception error) { file.failWrite(stream); throw error; }
        Entry saved = read();
        if (saved == null || !saved.ticket.equals(entry.ticket) || !saved.uri.equals(entry.uri) ||
            saved.acquired != entry.acquired || !saved.phase.equals(entry.phase) || !saved.grantUri.equals(entry.grantUri) ||
            !java.util.Objects.equals(saved.prefix, entry.prefix) || !java.util.Objects.equals(saved.displayName, entry.displayName)) throw new IOException("Export metadata not saved");
    }
    public void clear() throws IOException {
        file.delete(); File base = file.getBaseFile();
        if (base.exists() || new File(base + ".new").exists() || new File(base + ".bak").exists())
            throw new IOException("Export metadata not removed");
    }
}
