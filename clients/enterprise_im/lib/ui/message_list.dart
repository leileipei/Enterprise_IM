import 'package:flutter/material.dart';
import '../core/protocol/models.dart';
import '../core/protocol/seq.dart';
import '../core/sync/receipt_queue.dart';
import '../core/sync/client_context.dart';
import 'receipt_badge.dart';

class MessageList extends StatefulWidget {
  final List<MessageDTO> messages;
  final String userId, kind;
  final Map<Seq, ReceiptSummary> summaries;
  final ClientContext context;
  final ReceiptQueue receipts;
  const MessageList({
    super.key,
    required this.messages,
    required this.userId,
    required this.kind,
    required this.summaries,
    required this.context,
    required this.receipts,
  });
  @override
  State<MessageList> createState() => _MessageListState();
}

class _MessageListState extends State<MessageList> with WidgetsBindingObserver {
  final _viewport = GlobalKey();
  final _rows = <Seq, GlobalKey>{};
  final _scroll = ScrollController();
  bool _scheduled = false;
  @override
  void initState() {
    super.initState();
    WidgetsBinding.instance.addObserver(this);
    _scroll.addListener(_schedule);
  }

  @override
  void didChangeMetrics() => _schedule();
  @override
  void didChangeDependencies() {
    super.didChangeDependencies();
    _schedule();
  }

  @override
  void didUpdateWidget(MessageList old) {
    super.didUpdateWidget(old);
    if (!old.context.sameScope(widget.context)) {
      for (final seq in _rows.keys)
        old.receipts.observe(seq, 0, old.context, connected: false);
      _rows.clear();
    }
    _schedule();
  }

  void _schedule() {
    if (_scheduled || !mounted) return;
    _scheduled = true;
    WidgetsBinding.instance.addPostFrameCallback((_) {
      _scheduled = false;
      if (mounted) _measure();
    });
  }

  void _measure() {
    final viewport = _viewport.currentContext?.findRenderObject();
    if (viewport is! RenderBox || !viewport.attached || !viewport.hasSize)
      return;
    final clip = viewport.localToGlobal(Offset.zero) & viewport.size;
    final visible = widget.messages.map((m) => m.seq).toSet();
    for (final seq in _rows.keys.toList()) {
      final render = _rows[seq]!.currentContext?.findRenderObject();
      double ratio = 0;
      if (visible.contains(seq) &&
          render is RenderBox &&
          render.attached &&
          render.hasSize) {
        final rect = render.localToGlobal(Offset.zero) & render.size,
            intersection = rect.intersect(clip);
        if (rect.width > 0 && rect.height > 0 && !intersection.isEmpty)
          ratio =
              (intersection.width *
                      intersection.height /
                      (rect.width * rect.height))
                  .clamp(0, 1);
      }
      widget.receipts.observe(
        seq,
        ratio,
        widget.context,
        connected: visible.contains(seq),
      );
      if (!visible.contains(seq)) _rows.remove(seq);
    }
  }

  @override
  void dispose() {
    WidgetsBinding.instance.removeObserver(this);
    _scroll.removeListener(_schedule);
    _scroll.dispose();
    for (final seq in _rows.keys)
      widget.receipts.observe(seq, 0, widget.context, connected: false);
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    _schedule();
    return ClipRect(
      key: _viewport,
      child: NotificationListener<ScrollMetricsNotification>(
        onNotification: (_) {
          _schedule();
          return false;
        },
        child: ListView.builder(
          controller: _scroll,
          padding: EdgeInsets.zero,
          itemCount: widget.messages.length,
          itemBuilder: (context, index) {
            final message = widget.messages[index],
                own = message.senderUserId == widget.userId;
            final rowKey = _rows.putIfAbsent(message.seq, GlobalKey.new);
            return Container(
              key: rowKey,
              child: Container(
                key: ValueKey('message-${message.seq}'),
                padding: const EdgeInsets.all(12),
                child: Align(
                  alignment: own ? Alignment.centerRight : Alignment.centerLeft,
                  child: Container(
                    constraints: const BoxConstraints(maxWidth: 600),
                    padding: const EdgeInsets.all(12),
                    decoration: BoxDecoration(
                      color: own
                          ? Theme.of(context).colorScheme.primaryContainer
                          : Theme.of(context).colorScheme.surfaceContainer,
                      borderRadius: BorderRadius.circular(12),
                    ),
                    child: Column(
                      mainAxisSize: MainAxisSize.min,
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        if (message.redacted)
                          const Text('消息正文已不可用')
                        else ...[
                          Text(
                            own ? '我' : '收到的消息',
                            style: Theme.of(context).textTheme.labelSmall,
                          ),
                          if (message.messageType == 'text')
                            Text(message.text!)
                          else ...[
                            Text(
                              message.attachment?['available'] == true
                                  ? message.attachment!['original_filename']!
                                        as String
                                  : '附件已不可用',
                            ),
                            if (message.caption?.isNotEmpty == true)
                              Text(message.caption!),
                          ],
                          if (own)
                            ReceiptBadge(
                              summary: widget.summaries[message.seq],
                              kind: widget.kind,
                            ),
                        ],
                      ],
                    ),
                  ),
                ),
              ),
            );
          },
        ),
      ),
    );
  }
}
