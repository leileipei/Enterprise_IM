package com.enterpriseim.client;

import android.database.Cursor;
import android.net.Uri;
import android.os.Bundle;
import android.provider.DocumentsContract;
import java.io.IOException;

/** Only a complete directory listing can establish absence or a unique allocation. */
public final class ExportDirectory {
    private ExportDirectory() {}
    private static void complete(Cursor cursor) throws IOException {
        if (cursor == null) throw new IOException("Directory listing unavailable");
        Bundle extras = cursor.getExtras();
        if (extras == null) return;
        Object loading = extras.get(DocumentsContract.EXTRA_LOADING);
        if ((loading != null && !(loading instanceof Boolean)) || Boolean.TRUE.equals(loading) ||
            extras.get(DocumentsContract.EXTRA_ERROR) != null)
            throw new IOException("Directory listing incomplete");
    }
    public static Uri findAllocated(Cursor cursor, Uri tree, String prefix) throws IOException {
        complete(cursor);
        Uri found = null;
        while (cursor.moveToNext()) {
            String name = cursor.getString(1);
            if (name != null && name.startsWith(prefix)) {
                String id = cursor.getString(0);
                if (found != null || id == null || id.isEmpty() ||
                    DocumentsContract.Document.MIME_TYPE_DIR.equals(cursor.getString(2)))
                    throw new IOException("Ambiguous export allocation");
                found = DocumentsContract.buildDocumentUriUsingTree(tree, id);
            }
        }
        complete(cursor);
        return found;
    }
}
