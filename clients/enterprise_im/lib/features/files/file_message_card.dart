import 'dart:async';
import 'package:flutter/material.dart';
import '../../core/protocol/models.dart';
import '../../core/sync/client_context.dart';
import 'file_download.dart';

class FileMessageCard extends StatefulWidget {
  final MessageDTO message;
  final ClientContext context;
  final FileDownload download;
  final Duration Function() authorizationRemaining;
  const FileMessageCard({
    super.key,
    required this.message,
    required this.context,
    required this.download,
    required this.authorizationRemaining,
  });
  @override
  State<FileMessageCard> createState() => _FileMessageCardState();
}

class _FileMessageCardState extends State<FileMessageCard> {
  DownloadedArtifact? _artifact;
  bool _busy = false;
  String? _notice;
  Future<void> _get({bool export = false}) async {
    if (_busy) return;
    setState(() => _busy = true);
    try {
      final artifact = export && _artifact != null
          ? _artifact!
          : await widget.download.downloadAuthorized(
              widget.context,
              widget.message,
              authorizationRemaining: widget.authorizationRemaining,
            );
      if (!mounted) return;
      _artifact = artifact;
      if (export) {
        final saved = await widget.download.exportPrivateFile(
          widget.context,
          artifact,
        );
        if (mounted) _notice = saved ? '已导出到选择的位置' : '已取消导出';
      } else
        _notice = '已下载到本机私有缓存，导出需重新验证授权';
    } catch (_) {
      if (mounted) {
        _artifact = null;
        _notice = '文件操作未完成或授权已失效，请刷新消息后重试。';
      }
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  void didUpdateWidget(FileMessageCard old) {
    super.didUpdateWidget(old);
    if (!identical(old.message, widget.message) ||
        !old.context.sameScope(widget.context)) {
      _artifact = null;
      _notice = null;
    }
  }

  @override
  Widget build(BuildContext context) {
    final card = widget.message.attachment!,
        available = card['available'] == true;
    final allowed =
        available &&
        card['download_available'] == true &&
        widget.authorizationRemaining() > Duration.zero;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      mainAxisSize: MainAxisSize.min,
      children: [
        Text(available ? card['original_filename']! as String : '附件已不可用'),
        if (widget.message.caption?.isNotEmpty == true)
          Text(widget.message.caption!),
        if (available) Text('${card['actual_size_bytes']} 字节'),
        if (_busy) const LinearProgressIndicator(),
        if (_notice != null) Semantics(liveRegion: true, child: Text(_notice!)),
        if (allowed)
          Wrap(
            spacing: 8,
            children: [
              TextButton(
                onPressed: _busy ? null : () => _get(),
                child: const Text('下载附件'),
              ),
              TextButton(
                onPressed: _busy ? null : () => _get(export: true),
                child: const Text('导出附件'),
              ),
            ],
          )
        else if (available)
          const Text('当前不可下载'),
      ],
    );
  }
}
