import 'dart:async';
import 'package:flutter/material.dart';
import '../core/protocol/models.dart';
import '../features/conversations/conversation_view.dart';
import 'message_list.dart';
import 'message_composer.dart';
import 'receipt_badge.dart';
import '../features/files/file_download.dart';
import '../features/files/file_upload_panel.dart';
import '../platform/file_picker.dart';

class MessagePage extends StatefulWidget {
  final ConversationView view;
  final ConversationSummary conversation;
  final VoidCallback onBack;
  final FileDownload Function()? createDownloads;
  const MessagePage({
    super.key,
    required this.view,
    required this.conversation,
    required this.onBack,
    this.createDownloads,
  });
  @override
  State<MessagePage> createState() => _MessagePageState();
}

class _MessagePageState extends State<MessagePage> {
  String? _notice;
  FileDownload? _downloads;
  int _draft = 0;
  bool _leaving = false, _allowLeave = false;
  @override
  void initState() {
    super.initState();
    widget.view.addListener(_changed);
    _downloads =
        widget.createDownloads?.call() ?? FileDownload(widget.view.memberships);
  }

  void _changed() {
    if (widget.view.context == null || !widget.view.activity.foreground) {
      final downloads = _downloads;
      _downloads = null;
      if (downloads != null) unawaited(downloads.close());
    } else {
      _downloads ??=
          widget.createDownloads?.call() ??
          FileDownload(widget.view.memberships);
      unawaited(_downloads!.prune().catchError((Object _) {}));
    }
    if (mounted) setState(() {});
  }

  @override
  void dispose() {
    widget.view.removeListener(_changed);
    final downloads = _downloads;
    if (downloads != null) unawaited(downloads.close());
    unawaited(widget.view.close());
    super.dispose();
  }

  Future<void> _operation(Future<void> Function() action) async {
    if (mounted) setState(() => _notice = null);
    try {
      await action();
      if (mounted)
        setState(() {
          _notice = '已发送';
          _draft++;
        });
    } catch (_) {
      if (mounted) setState(() => _notice = '发送结果未确认，请重试原消息或明确放弃。');
      rethrow;
    }
  }

  Future<bool> _confirm(String title, String text, String accept) async {
    final activity = widget.view.activity, wasBlocked = activity.blocked;
    activity.update(
      foreground: activity.foreground,
      focused: activity.focused,
      activeConversation: activity.activeConversation,
      blocked: true,
    );
    try {
      return await showDialog<bool>(
            context: context,
            builder: (context) => AlertDialog(
              title: Text(title),
              content: Text(text),
              actions: [
                TextButton(
                  onPressed: () => Navigator.pop(context, false),
                  child: const Text('取消'),
                ),
                FilledButton(
                  onPressed: () => Navigator.pop(context, true),
                  child: Text(accept),
                ),
              ],
            ),
          ) ??
          false;
    } finally {
      activity.update(
        foreground: activity.foreground,
        focused: activity.focused,
        activeConversation: activity.activeConversation,
        blocked: wasBlocked,
      );
    }
  }

  Future<void> _leave() async {
    final view = widget.view;
    if (_leaving || view.sending) return;
    _leaving = true;
    try {
      if (view.pending.isNotEmpty &&
          !await _confirm('返回会话列表', '该消息的发送结果尚未确认。保留原请求，回到本会话后可显式重试。', '保留并返回'))
        return;
      if (!mounted) return;
      setState(() => _allowLeave = true);
      // PopScope must publish the new permission before Navigator.pop.
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted) widget.onBack();
      });
    } finally {
      _leaving = false;
    }
  }

  Future<void> _abandon(String id) async {
    if (!await _confirm('放弃原消息', '服务端可能已接收该消息。放弃仅取消本地重试，不会撤回已接收的消息。', '确认放弃'))
      return;
    if (!mounted) return;
    try {
      await widget.view.abandonPending(id);
      if (mounted)
        setState(() {
          _draft++;
          _notice = '已取消本地重试。';
        });
    } catch (_) {
      if (mounted) setState(() => _notice = '本地重试清理未完成，请重试。');
    }
  }

  @override
  Widget build(BuildContext context) {
    final view = widget.view, client = view.context;
    return PopScope<void>(
      canPop: _allowLeave || (!view.sending && view.pending.isEmpty),
      onPopInvokedWithResult: (didPop, _) {
        if (!didPop)
          unawaited(_leave());
        else {
          final activity = view.activity;
          activity.update(
            foreground: activity.foreground,
            focused: activity.focused,
            activeConversation: null,
            blocked: activity.blocked,
          );
          unawaited(view.close());
        }
      },
      child: Scaffold(
        appBar: AppBar(
          automaticallyImplyLeading: false,
          leading: IconButton(
            tooltip: '返回会话列表',
            onPressed: view.sending ? null : _leave,
            icon: const Icon(Icons.arrow_back),
          ),
          title: Text(widget.conversation.name),
          actions: [
            IconButton(
              tooltip: '刷新消息',
              onPressed: client == null || view.loading
                  ? null
                  : () async {
                      try {
                        await view.refresh();
                        await view.refreshMetadata();
                      } catch (_) {}
                    },
              icon: const Icon(Icons.refresh),
            ),
          ],
        ),
        body: SafeArea(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              if (view.loading) const LinearProgressIndicator(),
              Padding(
                padding: const EdgeInsets.symmetric(
                  horizontal: 16,
                  vertical: 8,
                ),
                child: UnreadBadge(state: view.unread),
              ),
              if (_notice != null || view.failure != null)
                Padding(
                  padding: const EdgeInsets.all(12),
                  child: Semantics(
                    liveRegion: true,
                    child: Text(
                      client == null && view.failure != null
                          ? '会话授权已失效，请返回列表重新验证。'
                          : _notice ?? '连接或状态查询未完成，请刷新重试。',
                    ),
                  ),
                ),
              Expanded(
                child: client == null
                    ? Center(
                        child: Text(
                          view.loading ? '正在验证会话访问权限' : '会话已关闭，请返回列表。',
                        ),
                      )
                    : MessageList(
                        messages: view.messages,
                        userId: client.session.userId,
                        kind: widget.conversation.kind,
                        summaries: view.summaries,
                        context: client,
                        receipts: view.receipts,
                        download: _downloads,
                        authorizationRemaining: view.fileAuthorizationRemaining,
                      ),
              ),
              for (final pending in view.pending)
                Padding(
                  padding: const EdgeInsets.all(12),
                  child: Wrap(
                    spacing: 8,
                    runSpacing: 8,
                    crossAxisAlignment: WrapCrossAlignment.center,
                    children: [
                      const Text('原消息发送结果未确认'),
                      FilledButton.tonal(
                        onPressed: view.sending || client == null
                            ? null
                            : () async {
                                try {
                                  await _operation(
                                    () =>
                                        view.retryPending(pending.clientMsgId),
                                  );
                                } catch (_) {}
                              },
                        child: const Text('重试原消息'),
                      ),
                      TextButton(
                        onPressed: view.sending
                            ? null
                            : () => _abandon(pending.clientMsgId),
                        child: const Text('放弃本地重试'),
                      ),
                    ],
                  ),
                ),
              if (client != null)
                FileUploadPanel(
                  key: ValueKey(client),
                  transfer: view.files,
                  context: client,
                  enabled: view.canManageFiles,
                  bind: (caption) async {
                    await _operation(() => view.bindFile(caption));
                    return null;
                  },
                  onChanged: view.reloadPending,
                  pick: () async {
                    final activity = view.activity,
                        wasBlocked = activity.blocked;
                    activity.update(
                      foreground: activity.foreground,
                      focused: activity.focused,
                      activeConversation: activity.activeConversation,
                      blocked: true,
                    );
                    try {
                      return await FilePicker().pick();
                    } finally {
                      activity.update(
                        foreground: activity.foreground,
                        focused: activity.focused,
                        activeConversation: activity.activeConversation,
                        blocked: wasBlocked,
                      );
                    }
                  },
                ),
              Padding(
                padding: const EdgeInsets.all(12),
                child: MessageComposer(
                  key: ValueKey(_draft),
                  contextIdentity: client ?? view,
                  enabled: view.canSend,
                  onSend: (text) => _operation(() => view.sendText(text)),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
