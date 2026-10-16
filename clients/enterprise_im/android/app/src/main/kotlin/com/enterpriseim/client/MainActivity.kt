package com.enterpriseim.client

import android.app.Activity
import android.app.AlertDialog
import android.content.Intent
import android.os.Build
import io.flutter.embedding.android.FlutterActivity
import io.flutter.embedding.engine.FlutterEngine
import io.flutter.plugin.common.MethodChannel
import java.util.UUID
import java.util.concurrent.atomic.AtomicBoolean

class MainActivity : FlutterActivity() {
    private val exportRequest = 6102
    private val owner = UUID.randomUUID().toString()
    private data class Choice(val reply: MethodChannel.Result, val name: String, val recovery: ExportLedger.Reauthorization? = null)
    private var choosing: Choice? = null
    private var recoveryPrompt: AlertDialog? = null
    private var closed = false
    private val recoveryOwnerOpen = AtomicBoolean(true)
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
    private fun cancelChoice(choice: Choice) {
        if (closed || choosing !== choice) return
        choosing = null
        exports.worker.execute { exports.cancel(owner) }
        choice.reply.success(null)
    }
    private fun requestDirectory(choice: Choice) {
        if (closed) { exports.worker.execute { exports.cancel(owner) }; return }
        try {
            startActivityForResult(Intent(Intent.ACTION_OPEN_DOCUMENT_TREE).apply {
                addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION or Intent.FLAG_GRANT_WRITE_URI_PERMISSION or Intent.FLAG_GRANT_PERSISTABLE_URI_PERMISSION or Intent.FLAG_GRANT_PREFIX_URI_PERMISSION)
                putExtra(Intent.EXTRA_TITLE, choice.name)
                if (choice.recovery != null && Build.VERSION.SDK_INT >= 26) putExtra("android.provider.extra.INITIAL_URI", choice.recovery.tree)
            }, exportRequest)
        } catch (failure: Exception) {
            choosing = null
            exports.worker.execute { exports.cancel(owner) }
            error(choice.reply, failure)
        }
    }
    private fun requestRecovery(choice: Choice) {
        recoveryPrompt = AlertDialog.Builder(this)
            .setTitle("重新授权原导出目录")
            .setMessage("上次导出尚未清理，需要重新选择原目录。清理成功前不会读取新的文件内容。")
            .setPositiveButton("继续清理") { _, _ -> if (!closed && choosing === choice) requestDirectory(choice) }
            .setNegativeButton("取消") { _, _ -> cancelChoice(choice) }
            .setOnCancelListener { cancelChoice(choice) }
            .show()
    }
    override fun configureFlutterEngine(engine: FlutterEngine) {
        super.configureFlutterEngine(engine)
        exports.worker.execute { exports.reconcileStartup() }
        MethodChannel(engine.dartExecutor.binaryMessenger, "enterprise_im/file_export").setMethodCallHandler { call, result ->
            if (call.method == "beginExport") {
                val name = call.argument<String>("name") ?: ""
                if (name.isBlank() || name != name.trim() || name.toByteArray(Charsets.UTF_8).size > 255 || name == "." || name == ".." || name.any { it.isISOControl() || it in "/\\:" }) {
                    result.error("invalid_filename", "Invalid filename", null)
                } else if (choosing != null || closed) {
                    result.error("export_busy", "Another export is active", null)
                } else {
                    choosing = Choice(result, name)
                    exports.worker.execute {
                        try {
                            exports.reserve(owner)
                            runOnUiThread { if (!closed) choosing?.let { requestDirectory(it) } else exports.worker.execute { exports.cancel(owner) } }
                        } catch (failure: Exception) {
                            runOnUiThread {
                                if (!closed) {
                                    if (failure is ExportManager.ReauthorizationException) {
                                        val recovery = Choice(result, name, failure.request)
                                        choosing = recovery
                                        requestRecovery(recovery)
                                    } else { choosing = null; error(result, failure) }
                                }
                            }
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
        // Directory selection creates no app-owned file. An abandoned result needs no deletion.
        val choice = choosing ?: return
        val reply = choice.reply
        val uri = data?.data
        if (choice.recovery != null) {
            if (resultCode != Activity.RESULT_OK || uri == null) { cancelChoice(choice); return }
            if (uri != choice.recovery.tree) {
                choosing = null
                reply.error("export_cleanup_failed", "所选目录不是原导出目录，未进行清理，请重试。", null)
                return
            }
            exports.worker.execute {
                try {
                    exports.reauthorizeAndReserve(owner, choice.recovery, uri, data.flags, recoveryOwnerOpen::get)
                    runOnUiThread {
                        if (closed) exports.worker.execute { exports.cancel(owner) }
                        else {
                            val next = Choice(reply, choice.name)
                            choosing = next
                            requestDirectory(next)
                        }
                    }
                } catch (failure: Exception) {
                    runOnUiThread { if (!closed && choosing === choice) { choosing = null; error(reply, failure) } }
                }
            }
            return
        }
        choosing = null
        exports.worker.execute {
            if (resultCode != Activity.RESULT_OK || uri == null) { exports.cancel(owner); reply.success(null) }
            else if (uri.scheme != "content") { exports.cancel(owner); reply.error("export_unavailable", "Invalid export destination", null) }
            else try { reply.success(exports.accept(owner, uri, data.flags, choice.name)) }
            catch (failure: Exception) { error(reply, failure) }
        }
    }
    override fun onDestroy() {
        recoveryOwnerOpen.set(false)
        closed = true
        recoveryPrompt?.dismiss()
        recoveryPrompt = null
        choosing?.reply?.error("export_cancelled", "Activity closed", null)
        choosing = null
        exports.worker.execute { exports.destroy(owner) }
        super.onDestroy()
    }
}
