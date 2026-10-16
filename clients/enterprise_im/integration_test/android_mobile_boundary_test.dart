import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:integration_test/integration_test.dart';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'package:enterprise_im/platform/login_callback.dart';
import 'package:enterprise_im/platform/mobile_activity.dart';

// Real Android VIEW / HOME / MAIN events from an owned host controller.
// Synthetic OAuth values only: no identity provider, tokens or backend fixture.
void main() {
  final binding = IntegrationTestWidgetsFlutterBinding.ensureInitialized();
  final source = AppLinksSource();
  final adapter = mobileActivityAdapter();
  const probe = String.fromEnvironment('IM_ANDROID_BOUNDARY_PROBE_ID');
  testWidgets('real cold and warm callbacks and foreground transitions', (
    tester,
  ) async {
    expect(Platform.isAndroid, isTrue);
    expect(probe, matches(RegExp(r'^[a-f0-9]{32}$')));
    addTearDown(source.close);
    addTearDown(adapter.dispose);
    await tester.runAsync(source.initialize);
    await tester.pumpWidget(
      const MaterialApp(home: Scaffold(body: Text('登录回调与前后台验证'))),
    );
    await tester.runAsync(() async {
      final file = File('${Directory.systemTemp.path}/im-boundary-$probe.json');
      final callbacks = <MobileLoginCallback>[];
      var coldInitial = false, cancelled = false;
      final lifecycle = <String>[];
      var backgroundMs = 0;
      Uri link(String state, String code, {String path = '/oauth/callback'}) =>
          Uri.parse('com.enterpriseim.client:$path?state=$state&code=$code');
      Future<void> stage(String phase, {String status = 'running'}) async {
        final temp = File('${file.path}.new');
        await temp.writeAsString(
          jsonEncode({
            'phase': phase,
            'status': status,
            'pid': pid,
            'cold_initial': coldInitial,
            'cancelled': cancelled,
            'lifecycle': lifecycle,
            'background_ms': backgroundMs,
          }),
          flush: true,
        );
        await temp.rename(file.path);
      }

      Future<void> until(bool Function() ready) async {
        final end = DateTime.now().add(const Duration(seconds: 35));
        while (!ready() && DateTime.now().isBefore(end)) {
          await Future<void>.delayed(const Duration(milliseconds: 20));
        }
        expect(ready(), isTrue, reason: 'real OS event deadline');
      }

      Future<LoginAttempt> start(String state) async {
        final callback = MobileLoginCallback(state, source);
        callbacks.add(callback);
        return callback.start();
      }

      try {
        final cold = link('cold-$probe', 'cold');
        expect(source.initial, cold);
        final initial = await start('cold-$probe');
        expect(
          await initial.callback.timeout(const Duration(seconds: 5)),
          cold,
        );
        coldInitial = true;
        await callbacks.last.close();
        final warm = await start('warm-$probe');
        var completed = false;
        unawaited(
          warm.callback.then((_) {
            completed = true;
          }),
        );
        final invalid = {
          'wrong-state': link('old-$probe', 'wrong'),
          'wrong-path': link('warm-$probe', 'wrong', path: '/oauth/wrong'),
          'duplicate-state': Uri.parse(
            'com.enterpriseim.client:/oauth/callback?state=warm-$probe&state=warm-$probe&code=wrong',
          ),
        };
        for (final entry in invalid.entries) {
          await stage(entry.key);
          await until(() => source.initial == entry.value);
          await Future<void>.delayed(const Duration(milliseconds: 100));
          expect(completed, isFalse, reason: entry.key);
        }
        await stage('warm-valid');
        expect(
          await warm.callback.timeout(const Duration(seconds: 35)),
          link('warm-$probe', 'warm'),
        );
        await callbacks.last.close();
        final abandoned = await start('cancel-$probe');
        final cancellation = expectLater(
          abandoned.callback,
          throwsA(isA<AuthCancelled>()),
        );
        await callbacks.last.close();
        await cancellation;
        cancelled = true;
        final next = await start('next-$probe');
        var nextCompleted = false;
        unawaited(
          next.callback.then((_) {
            nextCompleted = true;
          }),
        );
        await stage('stale-after-cancel');
        await until(() => source.initial == link('cancel-$probe', 'stale'));
        await Future<void>.delayed(const Duration(milliseconds: 100));
        expect(nextCompleted, isFalse);
        await stage('new-valid');
        expect(
          await next.callback.timeout(const Duration(seconds: 35)),
          link('next-$probe', 'next'),
        );
        await callbacks.last.close();
        await until(
          () =>
              binding.lifecycleState == AppLifecycleState.resumed &&
              adapter.activity.foreground &&
              adapter.activity.focused,
        );
        await stage('home-ready');
        await until(() => binding.lifecycleState == AppLifecycleState.paused);
        expect(adapter.activity.foreground, isFalse);
        expect(adapter.activity.focused, isFalse);
        lifecycle.add('paused');
        final clock = Stopwatch()..start();
        await Future<void>.delayed(const Duration(milliseconds: 1100));
        expect(binding.lifecycleState, AppLifecycleState.paused);
        expect(adapter.activity.foreground, isFalse);
        expect(adapter.activity.focused, isFalse);
        backgroundMs = clock.elapsedMilliseconds;
        await stage('background');
        await until(() => binding.lifecycleState == AppLifecycleState.resumed);
        expect(adapter.activity.foreground, isTrue);
        expect(adapter.activity.focused, isTrue);
        lifecycle.add('resumed');
        await stage('done', status: 'passed');
      } catch (_) {
        await stage('failed', status: 'failed');
        rethrow;
      } finally {
        for (final callback in callbacks) {
          await callback.close();
        }
      }
    });
  }, timeout: const Timeout(Duration(minutes: 5)));
}
