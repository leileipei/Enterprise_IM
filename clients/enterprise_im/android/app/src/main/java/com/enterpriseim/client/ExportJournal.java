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
        public final String ticket, phase;
        public final Uri uri;
        public final int acquired;
        public Entry(String ticket, Uri uri, int acquired, String phase) {
            this.ticket = ticket; this.uri = uri; this.acquired = acquired; this.phase = phase;
        }
        Entry phase(String next) { return new Entry(ticket, uri, acquired, next); }
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
        String ticket = json.getString("ticket"), phase = json.getString("phase");
        Uri uri = Uri.parse(json.getString("uri")); int flags = json.getInt("acquired");
        if (json.getInt("version") != 1 || json.length() != 5 ||
            !UUID.fromString(ticket).toString().equals(ticket) ||
            !"content".equals(uri.getScheme()) || uri.getAuthority() == null ||
            flags < 0 || (flags & ~3) != 0 ||
            !(phase.equals("PREPARED") || phase.equals("PENDING") || phase.equals("COMMITTED") || phase.equals("DELETED")))
            throw new IOException("Invalid export metadata");
        return new Entry(ticket, uri, flags, phase);
    }
    public void write(Entry entry) throws Exception {
        JSONObject json = new JSONObject();
        json.put("version", 1).put("ticket", entry.ticket).put("uri", entry.uri.toString())
            .put("acquired", entry.acquired).put("phase", entry.phase);
        byte[] bytes = json.toString().getBytes(StandardCharsets.UTF_8);
        if (bytes.length > 16384) throw new IOException("Oversized export metadata");
        FileOutputStream stream = file.startWrite();
        try { stream.write(bytes); stream.getFD().sync(); file.finishWrite(stream); }
        catch (Exception error) { file.failWrite(stream); throw error; }
        Entry saved = read();
        if (saved == null || !saved.ticket.equals(entry.ticket) || !saved.uri.equals(entry.uri) ||
            saved.acquired != entry.acquired || !saved.phase.equals(entry.phase)) throw new IOException("Export metadata not saved");
    }
    public void clear() throws IOException {
        file.delete(); File base = file.getBaseFile();
        if (base.exists() || new File(base + ".new").exists() || new File(base + ".bak").exists())
            throw new IOException("Export metadata not removed");
    }
}
