import 'dart:async';
import 'package:flutter/material.dart';
import '../../core/protocol/api_client.dart';
import '../../core/sync/client_context.dart';
import '../../platform/file_picker.dart';
import 'file_transfer.dart';

class FileUploadPanel extends StatefulWidget {
  final FileTransfer transfer;
  final ClientContext context;
  final bool enabled;
  final Future<SelectedFile?> Function()? pick;
  final Future<Object?> Function(String caption) bind;
  final Future<void> Function() onChanged;
  const FileUploadPanel({
    super.key,
    required this.transfer,
    required this.context,
    required this.enabled,
    this.pick,
    required this.bind,
    required this.onChanged,
  });
  @override
  State<FileUploadPanel> createState() => _FileUploadPanelState();
}

class _FileUploadPanelState extends State<FileUploadPanel> {
  final _caption = TextEditingController();
  bool _busy = false, _restored = false;
  String? _notice;
  @override
  void initState() {
    super.initState();
    if (widget.enabled) {
      _restored = true;
      unawaited(_run(() => widget.transfer.restore(widget.context)));
    }
  }

  @override
  void didUpdateWidget(FileUploadPanel old) {
    super.didUpdateWidget(old);
    if (!_restored && widget.enabled) {
      _restored = true;
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted && widget.enabled)
          unawaited(_run(() => widget.transfer.restore(widget.context)));
      });
    }
  }

  Future<void> _run(Future<void> Function() work) async {
    if (_busy) return;
    setState(() => _busy = true);
    try {
      await work();
    } catch (e) {
      if (mounted)
        _notice = switch (e is ApiFailure ? e.code : null) {
          'upload_context_mismatch' => '请选择原文件继续，文件内容和名称须一致。',
          'file_not_ready' => '文件尚未通过扫描。',
          'file_too_large' => '附件须大于0且不超过25MiB。',
          'file_type_not_allowed' => '仅支持PDF、PNG、JPEG、TXT。',
          _ => '文件操作未确认，请重试原操作或明确放弃。',
        };
    } finally {
      await widget.onChanged();
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _pick() async {
    await _run(() async {
      final file = await (widget.pick?.call() ?? FilePicker().pick());
      if (file == null || !mounted) return;
      _notice = null;
      await widget.transfer.reserveAndUpload(widget.context, file);
    });
  }

  Future<void> _cancel() async {
    final accepted = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('放弃文件操作'),
        content: const Text('仅取消本地继续处理。服务端可能已接收文件，不会撤回已经发送的消息。'),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(context, false),
            child: const Text('取消'),
          ),
          FilledButton(
            onPressed: () => Navigator.pop(context, true),
            child: const Text('确认放弃'),
          ),
        ],
      ),
    );
    if (accepted == true && mounted)
      await _run(() async {
        await widget.transfer.cancel();
        _notice = '已放弃本地文件操作';
      });
  }

  @override
  void dispose() {
    _caption.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final file = widget.transfer.current;
    return Padding(
      padding: const EdgeInsets.symmetric(horizontal: 12),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          if (_busy) const LinearProgressIndicator(),
          if (_notice != null)
            Semantics(liveRegion: true, child: Text(_notice!)),
          Wrap(
            spacing: 8,
            children: [
              TextButton(
                onPressed: _busy || !widget.enabled ? null : _pick,
                child: const Text('选择附件'),
              ),
              if (_busy)
                TextButton(
                  onPressed: widget.transfer.stopRequest,
                  child: const Text('停止传输'),
                ),
              if (!_busy)
                TextButton(
                  onPressed: widget.enabled ? _cancel : null,
                  child: const Text('放弃文件操作'),
                ),
            ],
          ),
          if (file != null) ...[
            Text('${file.name} · ${file.size} 字节'),
            Text(switch (file.state) {
              'allocated' => '等待上传，重新选择原文件继续',
              'uploaded' || 'scanning' => '等待扫描',
              'ready' => '扫描通过，可发送',
              'rejected' => '扫描拒绝，不能发送',
              'scan_failed' => '扫描失败，请稍后查询',
              _ => '文件已不可用',
            }),
            if (file.state == 'ready')
              TextField(
                controller: _caption,
                maxLength: 16384,
                decoration: const InputDecoration(labelText: '附件说明（可选）'),
              ),
            Wrap(
              spacing: 8,
              children: [
                TextButton(
                  onPressed: _busy || !widget.enabled
                      ? null
                      : () => _run(widget.transfer.poll),
                  child: const Text('查询扫描状态'),
                ),
                FilledButton(
                  onPressed: _busy || !widget.enabled || file.state != 'ready'
                      ? null
                      : () => _run(() async {
                          await widget.bind(_caption.text);
                          if (mounted) {
                            _notice = '附件已发送';
                            _caption.clear();
                          }
                        }),
                  child: const Text('发送附件'),
                ),
              ],
            ),
          ],
        ],
      ),
    );
  }
}
