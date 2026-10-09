import 'dart:async';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';

class MessageComposer extends StatefulWidget {
  final Object contextIdentity;
  final Future<void> Function(String) onSend;
  final bool enabled;
  const MessageComposer({
    super.key,
    required this.contextIdentity,
    required this.onSend,
    this.enabled = true,
  });
  @override
  State<MessageComposer> createState() => _MessageComposerState();
}

class _MessageComposerState extends State<MessageComposer> {
  final _text = TextEditingController();
  bool _busy = false;
  String? _notice;
  int _epoch = 0;
  bool get _composing =>
      _text.value.composing.isValid && !_text.value.composing.isCollapsed;
  bool get _canSend =>
      widget.enabled && !_busy && !_composing && _text.text.trim().isNotEmpty;
  @override
  void initState() {
    super.initState();
    _text.addListener(_edited);
  }

  void _edited() {
    if (mounted) setState(() {});
  }

  @override
  void didUpdateWidget(MessageComposer old) {
    super.didUpdateWidget(old);
    if (!identical(old.contextIdentity, widget.contextIdentity)) {
      _epoch++;
      _busy = false;
      _notice = null;
      _text.clear();
    } else if (old.enabled && !widget.enabled) {
      _epoch++;
      _busy = false;
    }
  }

  Future<void> _send() async {
    if (!_canSend) return;
    final epoch = ++_epoch, original = _text.text;
    setState(() {
      _busy = true;
      _notice = null;
    });
    try {
      await widget.onSend(original);
      if (mounted && epoch == _epoch && _text.text == original) _text.clear();
    } catch (_) {
      if (mounted && epoch == _epoch)
        setState(() => _notice = '发送结果未确认，请检查后重试。');
    } finally {
      if (mounted && epoch == _epoch) setState(() => _busy = false);
    }
  }

  KeyEventResult _key(FocusNode node, KeyEvent event) {
    if (event is! KeyDownEvent || event.logicalKey != LogicalKeyboardKey.enter)
      return KeyEventResult.ignored;
    final keyboard = HardwareKeyboard.instance;
    if (keyboard.isShiftPressed ||
        keyboard.isControlPressed ||
        keyboard.isMetaPressed ||
        keyboard.isAltPressed ||
        _composing)
      return KeyEventResult.ignored;
    if (!widget.enabled) return KeyEventResult.ignored;
    unawaited(_send());
    return KeyEventResult.handled;
  }

  @override
  void dispose() {
    _epoch++;
    _text.removeListener(_edited);
    _text.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => Padding(
    padding: const EdgeInsets.all(12),
    child: Column(
      mainAxisSize: MainAxisSize.min,
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (_notice != null) Semantics(liveRegion: true, child: Text(_notice!)),
        Row(
          crossAxisAlignment: CrossAxisAlignment.end,
          children: [
            Expanded(
              child: Focus(
                onKeyEvent: _key,
                child: TextField(
                  key: const ValueKey('message-editor'),
                  controller: _text,
                  enabled: widget.enabled && !_busy,
                  keyboardType: TextInputType.multiline,
                  textInputAction: TextInputAction.newline,
                  minLines: 1,
                  maxLines: 5,
                  decoration: const InputDecoration(
                    labelText: '消息',
                    hintText: '输入消息',
                    border: OutlineInputBorder(),
                  ),
                ),
              ),
            ),
            const SizedBox(width: 8),
            FilledButton(
              onPressed: _canSend ? _send : null,
              child: Text(_busy ? '发送中…' : '发送'),
            ),
          ],
        ),
      ],
    ),
  );
}
