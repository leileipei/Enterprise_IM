package com.enterpriseim.client;

import android.net.Uri;
import android.util.AtomicFile;
import org.json.JSONArray;
import org.json.JSONObject;
import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.ArrayList;

/** Owned permission release obligations; never contains payloads or deletes documents. */
public final class ExportGrantReleases {
    private static final int MAX_DIRECTORIES = 32, MAX_BYTES = 98304;
    private final AtomicFile file;
    private final ExportLedger.Resources resources;
    private static final class Receipt {
        final int owned, lastReleaseAttemptBoot;
        Receipt(int owned, int boot) { this.owned = owned; this.lastReleaseAttemptBoot = boot; }
    }
    public ExportGrantReleases(File directory, ExportLedger.Resources resources) {
        file = new AtomicFile(new File(directory, "export-grant-releases.json")); this.resources = resources;
    }
    private static void validate(Uri uri, int flags) throws IOException {
        if (uri == null || !"content".equals(uri.getScheme()) || uri.getAuthority() == null || uri.getAuthority().isEmpty() ||
            uri.toString().getBytes(StandardCharsets.UTF_8).length > 2048 || flags < 0 || (flags & ~3) != 0)
            throw new IOException("Invalid permission release metadata");
    }
    private LinkedHashMap<Uri, Receipt> read() throws Exception {
        File base = file.getBaseFile();
        if (!base.exists() && !new File(base + ".bak").exists()) {
            if (new File(base + ".new").exists()) throw new IOException("Incomplete permission release metadata");
            return new LinkedHashMap<>();
        }
        byte[] bytes;
        try (InputStream in = file.openRead(); ByteArrayOutputStream out = new ByteArrayOutputStream()) {
            byte[] block = new byte[1024]; int n;
            while ((n = in.read(block)) != -1) {
                if (out.size() + n > MAX_BYTES) throw new IOException("Oversized permission release metadata");
                out.write(block, 0, n);
            }
            bytes = out.toByteArray();
        }
        JSONObject json = new JSONObject(new String(bytes, StandardCharsets.UTF_8));
        if (json.length() != 2 || !(json.get("version") instanceof Integer) || json.getInt("version") != 1)
            throw new IOException("Unknown permission release metadata");
        JSONArray rows = json.getJSONArray("entries");
        if (rows.length() > MAX_DIRECTORIES) throw new IOException("Too many permission release receipts");
        LinkedHashMap<Uri, Receipt> entries = new LinkedHashMap<>();
        for (int i = 0; i < rows.length(); i++) {
            JSONObject row = rows.getJSONObject(i);
            if (row.length() != 3 || !(row.get("uri") instanceof String) || !(row.get("owned") instanceof Integer) ||
                !(row.get("lastReleaseAttemptBoot") instanceof Integer)) throw new IOException("Invalid permission release receipt");
            Uri uri = Uri.parse(row.getString("uri")); int flags = row.getInt("owned"), boot = row.getInt("lastReleaseAttemptBoot");
            validate(uri, flags);
            if (flags == 0 || boot < -1 || entries.containsKey(uri)) throw new IOException("Invalid permission release receipt");
            entries.put(uri, new Receipt(flags, boot));
        }
        return entries;
    }
    private static JSONObject json(LinkedHashMap<Uri, Receipt> entries) throws Exception {
        JSONArray rows = new JSONArray();
        for (Uri uri : entries.keySet()) {
            Receipt receipt = entries.get(uri);
            rows.put(new JSONObject().put("uri", uri.toString()).put("owned", receipt.owned).put("lastReleaseAttemptBoot", receipt.lastReleaseAttemptBoot));
        }
        return new JSONObject().put("version", 1).put("entries", rows);
    }
    private void write(LinkedHashMap<Uri, Receipt> entries) throws Exception {
        byte[] bytes = json(entries).toString().getBytes(StandardCharsets.UTF_8);
        if (bytes.length > MAX_BYTES) throw new IOException("Oversized permission release metadata");
        FileOutputStream stream = file.startWrite();
        try { stream.write(bytes); stream.getFD().sync(); file.finishWrite(stream); }
        catch (Exception error) { file.failWrite(stream); throw error; }
        if (!json(entries).toString().equals(json(read()).toString())) throw new IOException("Permission release metadata not saved");
        // Android7 AtomicFile does not remove a reserved .new sidecar itself.
        File pending = new File(file.getBaseFile() + ".new");
        if (pending.exists() && !pending.delete()) throw new IOException("Permission release metadata incomplete");
    }
    public void ensureCapacity(Uri uri, int flags) throws Exception {
        validate(uri, flags); LinkedHashMap<Uri, Receipt> entries = read();
        if (flags != 0 && !entries.containsKey(uri) && entries.size() >= MAX_DIRECTORIES)
            throw new IOException("Permission release receipt capacity reached");
    }
    /** Called before take, after the export journal durably records the new ownership. */
    public void acquiring(Uri uri, int flags) throws Exception {
        ensureCapacity(uri, flags); if (flags == 0) return;
        LinkedHashMap<Uri, Receipt> entries = read(); Receipt previous = entries.get(uri);
        entries.put(uri, new Receipt(flags | (previous == null ? 0 : previous.owned), -1)); write(entries);
    }
    /** Transfer legacy journal ownership too, before clearing its terminal receipt. */
    public void release(Uri uri, int flags) throws Exception {
        ensureCapacity(uri, flags); LinkedHashMap<Uri, Receipt> entries = read(); Receipt previous = entries.get(uri);
        if (flags != 0) {
            entries.put(uri, new Receipt(flags | (previous == null ? 0 : previous.owned), previous == null ? -1 : previous.lastReleaseAttemptBoot)); write(entries);
        }
        reconcileOne(entries, uri);
    }
    private void reconcileOne(LinkedHashMap<Uri, Receipt> entries, Uri uri) throws Exception {
        Receipt receipt = entries.get(uri); if (receipt == null) return;
        int boot = resources.bootCount(), held = resources.grants(uri) & receipt.owned;
        if (boot >= 0 && receipt.lastReleaseAttemptBoot >= 0 && boot > receipt.lastReleaseAttemptBoot && held == 0) {
            entries.remove(uri); write(entries); return;
        }
        if (held != 0 || receipt.lastReleaseAttemptBoot == -1) {
            // Updating before every attempt prevents same-boot pruning after a resurrected grant is released again.
            entries.put(uri, new Receipt(receipt.owned, boot >= 0 ? boot : -1)); write(entries);
            if (held != 0) resources.release(uri, held);
            if ((resources.grants(uri) & receipt.owned) != 0) throw new IOException("URI release not confirmed");
        }
    }
    public void reconcile(Uri protectedUri) throws Exception {
        LinkedHashMap<Uri, Receipt> entries = read();
        for (Uri uri : new ArrayList<>(entries.keySet())) if (!uri.equals(protectedUri)) reconcileOne(entries, uri);
    }
}
