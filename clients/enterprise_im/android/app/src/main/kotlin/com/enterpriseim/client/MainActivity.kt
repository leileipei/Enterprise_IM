package com.enterpriseim.client

import android.app.Activity
import android.content.Intent
import android.net.Uri
import android.provider.DocumentsContract
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel
import java.io.OutputStream
import java.util.UUID

class MainActivity : FlutterActivity() {
    private val exportRequest = 6102
    private var choosing: MethodChannel.Result? = null
    private var active: Export? = null
    private data class Export(val ticket: String, val uri: Uri, val stream: OutputStream, var bytes: Long = 0)

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
                        export.stream.write(bytes)
                        export.bytes += bytes.size
                        result.success(null)
                    }
                    "finishExport" -> {
                        val export = requireExport(call.argument<String>("ticket"))
                        if (export.bytes < 1) throw IllegalArgumentException()
                        export.stream.flush()
                        export.stream.close()
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
        try { export.stream.close() } catch (_: Exception) {}
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
            val stream = contentResolver.openOutputStream(uri, "w") ?: throw IllegalStateException()
            val export = Export(UUID.randomUUID().toString(), uri, stream)
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
