package com.enterpriseim.client;

import java.io.IOException;
import java.io.OutputStream;

/** One owned destination; payload is private until the final copy. */
public final class ExportSession {
    public interface OpenOutput { OutputStream open() throws Exception; }
    public interface DeleteDocument { boolean delete() throws Exception; }
    private static final class Buffer {
        // Fixed blocks avoid a second full payload allocation when the buffer grows.
        private final byte[][] blocks = new byte[800][];
        private int size;
        int size() { return size; }
        void write(byte[] bytes) {
            int offset = 0;
            while (offset < bytes.length) {
                int index = size / 32768, used = size % 32768;
                if (blocks[index] == null) blocks[index] = new byte[32768];
                int count = Math.min(bytes.length - offset, 32768 - used);
                System.arraycopy(bytes, offset, blocks[index], used, count);
                offset += count; size += count;
            }
        }
        void writeTo(OutputStream stream) throws IOException {
            for (int index = 0, remaining = size; remaining > 0; index++) {
                int count = Math.min(remaining, 32768);
                stream.write(blocks[index], 0, count); remaining -= count;
            }
        }
        void erase() {
            for (int index = 0; index < blocks.length; index++) {
                if (blocks[index] != null) {
                    java.util.Arrays.fill(blocks[index], (byte) 0);
                    blocks[index] = null;
                }
            }
            size = 0;
        }
    }
    private final Buffer buffer = new Buffer();
    private final OpenOutput output;
    private final DeleteDocument delete;
    private boolean terminal;
    public ExportSession(OpenOutput output, DeleteDocument delete) { this.output = output; this.delete = delete; }
    public void write(byte[] bytes) throws IOException {
        if (terminal || bytes.length > 32768 || (long)buffer.size() + bytes.length > 26214400L) throw new IOException("Invalid export write");
        buffer.write(bytes);
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
