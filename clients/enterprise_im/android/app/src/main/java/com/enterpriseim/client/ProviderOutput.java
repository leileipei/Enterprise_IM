package com.enterpriseim.client;

import android.content.ContentResolver;
import android.content.res.AssetFileDescriptor;
import android.net.Uri;
import java.io.IOException;
import java.io.FilterOutputStream;
import java.io.OutputStream;

/** Preserve the provider's asset extent and close its descriptor with the stream. */
public final class ProviderOutput {
    public static OutputStream open(ContentResolver resolver, Uri uri) throws IOException {
        AssetFileDescriptor asset = resolver.openAssetFileDescriptor(uri, "w");
        if (asset == null) throw new IOException("Cannot open export destination");
        return wrap(asset);
    }
    public static OutputStream wrap(AssetFileDescriptor asset) throws IOException {
        try {
            return new FilterOutputStream(asset.createOutputStream()) {
                @Override public void close() throws IOException {
                    IOException failure = null;
                    // AutoCloseOutputStream does not check the reliable peer's
                    // status. Observe an already reported error before closing
                    // the descriptor; this is not a remote commit handshake.
                    try {
                        if (asset.getParcelFileDescriptor().canDetectErrors())
                            asset.getParcelFileDescriptor().checkError();
                    } catch (IOException error) { failure = error; }
                    try { super.close(); }
                    catch (IOException error) {
                        if (failure == null) failure = error;
                        else failure.addSuppressed(error);
                    }
                    if (failure != null) throw failure;
                }
                @Override public void write(byte[] bytes, int offset, int size) throws IOException {
                    out.write(bytes, offset, size);
                }
            };
        }
        catch (IOException error) { try { asset.close(); } catch (IOException close) { error.addSuppressed(close); } throw error; }
    }
}
