package com.enterpriseim.client;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.OutputStream;

/** One owned destination; payload is private until the final copy. */
public final class ExportSession {
    public interface OpenOutput { OutputStream open() throws Exception; }
    public interface DeleteDocument { boolean delete() throws Exception; }
    private static final class Buffer extends ByteArrayOutputStream {
        Buffer() { super(32768); }
        void erase() { java.util.Arrays.fill(buf, (byte) 0); reset(); }
    }
    private final Buffer buffer = new Buffer();
    private final OpenOutput output;
    private final DeleteDocument delete;
    private boolean terminal;
    public ExportSession(OpenOutput output, DeleteDocument delete) { this.output = output; this.delete = delete; }
    public void write(byte[] bytes) throws IOException {
        if (terminal || bytes.length > 32768 || (long)buffer.size() + bytes.length > 26214400L) throw new IOException("Invalid export write");
        buffer.write(bytes, 0, bytes.length);
    }
    public void finish() throws Exception {
        if (terminal || buffer.size() == 0) throw new IOException("Invalid export finish");
        terminal = true;
        try (OutputStream stream = output.open()) {
            if (stream == null) throw new IOException("Cannot open export destination");
            buffer.writeTo(stream);
            stream.flush();
        } finally { buffer.erase(); }
    }
    public void abort() throws Exception {
        terminal = true;
        buffer.erase();
        if (!delete.delete()) throw new IOException("Export destination cleanup failed");
    }
}
