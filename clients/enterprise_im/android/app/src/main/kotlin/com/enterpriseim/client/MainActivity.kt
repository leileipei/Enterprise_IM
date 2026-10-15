package com.enterpriseim.client

import android.app.Activity
import android.content.Intent
import android.net.Uri
import android.provider.DocumentsContract
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel
import java.io.ByteArrayOutputStream
import java.util.UUID

class MainActivity : FlutterActivity() {
    private val exportRequest = 6102
    private var choosing: MethodChannel.Result? = null
    private var active: Export? = null
    private class ExportBuffer : ByteArrayOutputStream(32768) {
        fun discard() { buf.fill(0); reset() }
    }
    private data class Export(val ticket: String, val uri: Uri, val buffer: ExportBuffer = ExportBuffer(), var bytes: Long = 0)

    override fun configureFlutterEngine(engine: FlutterEngine) {
        super.configureFlutterEngine(engine)
        MethodChannel(engine.dartExecutor.binaryMessenger, "enterprise_im/file_export").setMethodCallHandler { call, result ->
            try {
                when (call.method) {
                    "beginExport" -> {
                        if (choosing != null || active != null) { result.error("export_busy", "Another export is active", null); return@setMethodCallHandler }
                        val name = call.argument<String>("name") ?: ""
                        if (name.isBlank() || name != name.trim() || name.toByteArray(Charsets.UTF_8).size > 255 || name == "." || name == ".." || name.any { it.isISOControl() || it in "/\\:" }) {
                            result.error("invalid_filename", "Invalid filename", null); return@setMethodCallHandler
                        }
                        choosing = result
                        startActivityForResult(Intent(Intent.ACTION_CREATE_DOCUMENT).apply {
                            addCategory(Intent.CATEGORY_OPENABLE)
                            type = "application/octet-stream"
                            putExtra(Intent.EXTRA_TITLE, name)
                        }, exportRequest)
                    }
                    "writeExport" -> {
                        val export = requireExport(call.argument<String>("ticket"))
                        val bytes = call.argument<ByteArray>("bytes") ?: throw IllegalArgumentException()
                        if (bytes.size > 32768 || export.bytes + bytes.size > 26214400L) throw IllegalArgumentException()
                        export.buffer.write(bytes)
                        export.bytes += bytes.size
                        result.success(null)
                    }
                    "finishExport" -> {
                        val export = requireExport(call.argument<String>("ticket"))
                        if (export.bytes < 1) throw IllegalArgumentException()
                        // Publish only a complete source. Closing a partially written
                        // Downloads stream and immediately deleting its document can
                        // race the provider's asynchronous metadata update.
                        val stream = contentResolver.openOutputStream(export.uri, "w")
                            ?: throw IllegalStateException()
                        stream.use { export.buffer.writeTo(it); it.flush() }
                        export.buffer.discard()
                        active = null
                        result.success(null)
                    }
                    "abortExport" -> {
                        val export = active
                        if (export != null && call.argument<String>("ticket") == export.ticket) abort(export)
                        result.success(null)
                    }
                    else -> result.notImplemented()
                }
            } catch (_: Exception) {
                choosing = null
                active?.let { abort(it) }
                result.error("export_unavailable", "File export failed", null)
            }
        }
    }
    private fun requireExport(ticket: String?): Export {
        val export = active ?: throw IllegalStateException()
        if (ticket != export.ticket) throw IllegalArgumentException()
        return export
    }
    private fun abort(export: Export) {
        active = null
        export.buffer.discard()
        try { DocumentsContract.deleteDocument(contentResolver, export.uri) } catch (_: Exception) {}
    }
    @Deprecated("Deprecated in Android API")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        if (requestCode != exportRequest) { super.onActivityResult(requestCode, resultCode, data); return }
        val reply = choosing ?: return
        choosing = null
        val uri = data?.data
        if (resultCode != Activity.RESULT_OK || uri == null) { reply.success(null); return }
        if (uri.scheme != "content") { reply.error("export_unavailable", "Invalid export destination", null); return }
        try {
            // The selected document stays empty until finishExport. Partial
            // source errors, cancellation and size rejection only erase the
            // bounded private RAM buffer and delete the unopened document.
            val export = Export(UUID.randomUUID().toString(), uri)
            active = export
            reply.success(export.ticket)
        } catch (_: Exception) {
            try { DocumentsContract.deleteDocument(contentResolver, uri) } catch (_: Exception) {}
            reply.error("export_unavailable", "Cannot open export destination", null)
        }
    }
    override fun onDestroy() {
        choosing?.error("export_cancelled", "Activity closed", null)
        choosing = null
        active?.let { abort(it) }
        super.onDestroy()
    }
}
