package com.enterpriseim.client

import android.app.Activity
import android.content.Intent
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel
import java.util.UUID

class MainActivity : FlutterActivity() {
    private val exportRequest = 6102
    private val owner = UUID.randomUUID().toString()
    private var choosing: MethodChannel.Result? = null
    private var closed = false
    private val exports by lazy { ExportManager.get(applicationContext) }

    private fun error(result: MethodChannel.Result, failure: Exception) {
        if (failure is ExportManager.CleanupException) {
            result.error("export_cleanup_failed", "Export destination cleanup failed; retry required", null)
        } else if (failure is ExportManager.BusyException) {
            result.error("export_busy", "Another export is active", null)
        } else {
            result.error("export_unavailable", "File export failed", null)
        }
    }
    override fun configureFlutterEngine(engine: FlutterEngine) {
        super.configureFlutterEngine(engine)
        MethodChannel(engine.dartExecutor.binaryMessenger, "enterprise_im/file_export").setMethodCallHandler { call, result ->
            if (call.method == "beginExport") {
                val name = call.argument<String>("name") ?: ""
                if (name.isBlank() || name != name.trim() || name.toByteArray(Charsets.UTF_8).size > 255 || name == "." || name == ".." || name.any { it.isISOControl() || it in "/\\:" }) {
                    result.error("invalid_filename", "Invalid filename", null)
                } else if (choosing != null || closed) {
                    result.error("export_busy", "Another export is active", null)
                } else {
                    choosing = result
                    exports.worker.execute {
                        try {
                            exports.reserve(owner)
                            runOnUiThread {
                                if (closed) { exports.worker.execute { exports.cancel(owner) } }
                                else try {
                                    startActivityForResult(Intent(Intent.ACTION_CREATE_DOCUMENT).apply {
                                        addCategory(Intent.CATEGORY_OPENABLE)
                                        addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION or Intent.FLAG_GRANT_PERSISTABLE_URI_PERMISSION)
                                        type = "application/octet-stream"
                                        putExtra(Intent.EXTRA_TITLE, name)
                                    }, exportRequest)
                                } catch (failure: Exception) {
                                    choosing = null
                                    exports.worker.execute { exports.cancel(owner) }
                                    error(result, failure)
                                }
                            }
                        } catch (failure: Exception) {
                            runOnUiThread { if (!closed) { choosing = null; error(result, failure) } }
                        }
                    }
                }
            } else if (call.method in listOf("writeExport", "finishExport", "abortExport")) {
                exports.worker.execute {
                    try {
                        val ticket = call.argument<String>("ticket") ?: ""
                        when (call.method) {
                            "writeExport" -> exports.write(owner, ticket, call.argument<ByteArray>("bytes") ?: throw IllegalArgumentException())
                            "finishExport" -> exports.finish(owner, ticket)
                            "abortExport" -> exports.abort(owner, ticket)
                        }
                        result.success(null)
                    } catch (failure: Exception) { error(result, failure) }
                }
            } else result.notImplemented()
        }
    }
    @Deprecated("Deprecated in Android API")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        if (requestCode != exportRequest) { super.onActivityResult(requestCode, resultCode, data); return }
        val reply = choosing ?: return
        choosing = null
        val uri = data?.data
        exports.worker.execute {
            if (resultCode != Activity.RESULT_OK || uri == null) { exports.cancel(owner); reply.success(null) }
            else if (uri.scheme != "content") { exports.cancel(owner); reply.error("export_unavailable", "Invalid export destination", null) }
            else try { reply.success(exports.accept(owner, uri, data.flags)) }
            catch (failure: Exception) { error(reply, failure) }
        }
    }
    override fun onDestroy() {
        closed = true
        choosing?.error("export_cancelled", "Activity closed", null)
        choosing = null
        exports.worker.execute { exports.destroy(owner) }
        super.onDestroy()
    }
}
