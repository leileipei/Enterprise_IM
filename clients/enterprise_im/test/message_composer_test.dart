import 'dart:async';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/ui/message_composer.dart';

void main() {
  testWidgets('testImeCompositionDoesNotSend', (tester) async {
    final sent = <String>[], identity = Object();
    await tester.pumpWidget(
      MaterialApp(
        home: Scaffold(
          body: MessageComposer(
            contextIdentity: identity,
            onSend: (text) async => sent.add(text),
          ),
        ),
      ),
    );
    await tester.tap(find.byType(TextField));
    tester.testTextInput.updateEditingValue(
      const TextEditingValue(
        text: 'zhong',
        selection: TextSelection.collapsed(offset: 5),
        composing: TextRange(start: 0, end: 5),
      ),
    );
    await tester.pump();
    await tester.sendKeyEvent(LogicalKeyboardKey.enter);
    await tester.pump();
    expect(sent, isEmpty);
    tester.testTextInput.updateEditingValue(
      const TextEditingValue(
        text: '中文',
        selection: TextSelection.collapsed(offset: 2),
      ),
    );
    await tester.pump();
    await tester.sendKeyEvent(LogicalKeyboardKey.enter);
    await tester.pump();
    expect(sent, ['中文']);
    expect(
      tester.widget<TextField>(find.byType(TextField)).controller!.text,
      isEmpty,
    );
  });
  testWidgets(
    'pending callback blocks duplicates and failure retains original text',
    (tester) async {
      final result = Completer<void>();
      var calls = 0;
      await tester.pumpWidget(
        MaterialApp(
          home: Scaffold(
            body: MessageComposer(
              contextIdentity: Object(),
              onSend: (text) {
                calls++;
                return result.future;
              },
            ),
          ),
        ),
      );
      await tester.enterText(find.byType(TextField), '  原文  ');
      await tester.tap(find.text('发送'));
      await tester.pump();
      await tester.sendKeyEvent(LogicalKeyboardKey.enter);
      await tester.pump();
      expect(calls, 1);
      result.completeError(StateError('rejected'));
      await tester.pump();
      expect(
        tester.widget<TextField>(find.byType(TextField)).controller!.text,
        '  原文  ',
      );
      expect(find.textContaining('未确认'), findsOneWidget);
    },
  );
  testWidgets(
    'context switch fences late clear and Shift Enter does not send',
    (tester) async {
      final old = Completer<void>();
      final first = Object(), second = Object();
      var calls = 0;
      Widget ui(Object context) => MaterialApp(
        home: Scaffold(
          body: MessageComposer(
            contextIdentity: context,
            onSend: (_) {
              calls++;
              return old.future;
            },
          ),
        ),
      );
      await tester.pumpWidget(ui(first));
      await tester.enterText(find.byType(TextField), '旧会话');
      await tester.sendKeyDownEvent(LogicalKeyboardKey.shiftLeft);
      await tester.sendKeyEvent(LogicalKeyboardKey.enter);
      await tester.sendKeyUpEvent(LogicalKeyboardKey.shiftLeft);
      await tester.pump();
      expect(calls, 0);
      await tester.tap(find.text('发送'));
      await tester.pump();
      await tester.pumpWidget(ui(second));
      await tester.enterText(find.byType(TextField), '新草稿');
      old.complete();
      await tester.pump();
      expect(
        tester.widget<TextField>(find.byType(TextField)).controller!.text,
        '新草稿',
      );
    },
  );
}
