// Explicit integration entry point: requires the owned Go fixture config.
// Detects wire DTO drift, forged cross-device read and unread divergence.
import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:math';
import 'package:enterprise_im/core/auth/auth_session.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/protocol/models.dart';
import 'package:enterprise_im/core/protocol/seq.dart';
import 'package:enterprise_im/core/storage/encrypted_store.dart';
import 'package:enterprise_im/core/sync/sync_engine.dart';
import 'package:enterprise_im/core/sync/receipt_queue.dart';
import 'package:enterprise_im/core/sync/realtime_channel.dart';
import 'package:enterprise_im/platform/app_activity.dart';

void check(bool value, String name) {
  if (!value) throw StateError(name);
}

Future<void> until(bool Function() ready, String name) async {
  final elapsed = Stopwatch()..start();
  while (!ready()) {
    if (elapsed.elapsed > const Duration(seconds: 10)) throw StateError(name);
    await Future<void>.delayed(const Duration(milliseconds: 20));
  }
}

final class FixtureTrust extends HttpOverrides {
  final SecurityContext trust;
  FixtureTrust(this.trust);
  @override
  HttpClient createHttpClient(SecurityContext? _) =>
      super.createHttpClient(trust);
}

Future<void> main(List<String> arguments) async {
  if (arguments.length != 1) throw ArgumentError('owned config required');
  final config =
      jsonDecode(await File(arguments.single).readAsString())
          as Map<String, dynamic>;
  final trust = SecurityContext(withTrustedRoots: false)
    ..setTrustedCertificates(config['ca'] as String);
  await HttpOverrides.runWithHttpOverrides(
    () => probe(config),
    FixtureTrust(trust),
  );
}

Future<void> probe(Map<String, dynamic> config) async {
  final origin = Uri.parse(config['origin'] as String);
  final api = ApiClient(origin, HttpApiTransport());
  final root = Directory(config['storage'] as String);
  await root.create();
  final stores = <EncryptedStore>[];
  final engines = <SyncEngine>[];
  final activities = <AppActivity>[];
  final queues = <ReceiptQueue>[];
  RealtimeChannel? channel;
  StreamSubscription<RealtimeSignal>? signals;
  StreamSubscription<ApiFailure>? failures;
  var noticeCount = 0;
  final channelFailures = <ApiFailure>[];
  try {
    Future<ClientContext> register(String who, int slot) async {
      final data = config[who] as Map<String, dynamic>;
      final device = DeviceGrant.fromJson(
        await api.json(
          'POST',
          '/api/v1/devices',
          bearer: data['token'] as String,
          actingMembershipId: data['membership'] as String,
          body: {
            'registration_id': config['registrations'][slot],
            'platform': 'macos',
            'name': 'owned-protocol-$slot',
          },
        ),
      );
      return ClientContext(
        session: AuthSession(
          origin: origin,
          tenantId: config['tenant'] as String,
          userId: data['user'] as String,
          accessToken: data['token'] as String,
          expiresAt: DateTime.now().toUtc().add(const Duration(minutes: 10)),
          generation: 1,
          budget: const Duration(minutes: 10),
        ),
        actingMembershipId: data['membership'] as String,
        device: device,
        generation: 1,
        conversationId: config['conversation'] as String,
      );
    }

    final sender = await register('sender', 0);
    final a = await register('bob', 1), b = await register('bob', 2);
    check(a.device.deviceId != b.device.deviceId, 'independent devices');
    Future<Map<String, Object?>> request(
      String method,
      String suffix,
      ClientContext c, {
      Map<String, Object?>? body,
    }) => api.json(
      method,
      '/api/v1/conversations/${c.conversationId}/$suffix',
      body: body,
      bearer: c.session.accessToken,
      actingMembershipId: c.actingMembershipId,
      device: c.device,
    );
    Future<UnreadState> unread(ClientContext c) async =>
        UnreadState.fromJson(await request('GET', 'unread', c));
    Future<List<ReceiptSummary>> summaries() async {
      final data = await request('GET', 'receipts?seqs=1,2,3', sender);
      return (data['receipts'] as List)
          .map(
            (v) => ReceiptSummary.fromJson((v as Map).cast<String, Object?>()),
          )
          .toList();
    }

    channel = RealtimeChannel(api, HttpRealtimeConnector());
    signals = channel.signals.listen((signal) {
      if (signal == RealtimeSignal.syncRequired) noticeCount++;
    });
    failures = channel.failures.listen(channelFailures.add);
    await channel.connect(a);
    await until(
      () => channel!.connected || channelFailures.isNotEmpty,
      'real websocket ready',
    );
    check(
      channel.connected && channelFailures.isEmpty,
      'real websocket protocol',
    );
    for (var i = 0; i < 3; i++) {
      final result = await request(
        'POST',
        'messages',
        sender,
        body: {
          'client_msg_id': config['messageIds'][i],
          'message_type': 'text',
          'text': 'real-Go-body-${i + 1}',
        },
      );
      final ack = MessageACK.fromJson(result);
      check(ack.seq == Seq.parse('${i + 1}') && ack.conversationId == sender.conversationId && !ack.duplicate, 'sender acknowledged sequence');
    }
    await until(
      () => noticeCount > 0 || channelFailures.isNotEmpty,
      'real websocket notification',
    );
    check(channelFailures.isEmpty, 'realtime failure');
    check(
      (await unread(a)).count == 3 && (await unread(b)).count == 3,
      'initial unread',
    );
    for (var slot = 0; slot < 2; slot++) {
      final random = Random.secure();
      final store = await EncryptedStore.open(
        '${slot + 1}' * 64,
        List.generate(32, (_) => random.nextInt(256)),
        directory: root,
        generation: 1,
      );
      stores.add(store);
      engines.add(SyncEngine(api, store));
      final activity = AppActivity()
        ..update(
          foreground: true,
          focused: true,
          activeConversation: a.conversationId,
        );
      activities.add(activity);
      queues.add(ReceiptQueue(api, activity, onFailure: channelFailures.add));
    }
    final resultA = await engines[0].syncConversation(a);
    check(
      resultA.messages.length == 3 && resultA.deliveredSeqs.length == 3,
      'A page durable delivery',
    );
    check(
      await stores[0].committedCursor(a.conversationId) == Seq.fromWire('3') &&
          resultA.messages[0].text == 'real-Go-body-1',
      'durable cursor and authorized body',
    );
    queues[0].acceptDelivered(resultA, a);
    queues[0].observe(Seq.fromWire('1'), .49, a);
    await Future<void>.delayed(const Duration(milliseconds: 900));
    check((await unread(b)).count == 3, 'subthreshold visibility never reads');
    queues[0].observe(Seq.fromWire('1'), 1, a);
    await until(
      () =>
          queues[0].locallyReadSeqs.contains(Seq.fromWire('1')) ||
          channelFailures.isNotEmpty,
      'A visible read',
    );
    check(
      (await unread(b)).count == 2 && (await unread(b)).exact,
      'user read merges across devices',
    );
    var rejected = false;
    try {
      await request(
        'PUT',
        'read',
        b,
        body: {
          'seqs': ['1'],
        },
      );
    } on ApiFailure catch (error) {
      rejected = error.status == 404 && error.code == 'not_found';
    }
    check(rejected, 'user read cannot forge B delivery');
    var summary = await summaries();
    check(
      summary[0].status == 'read' &&
          summary[0].visibleReadCount == 1 &&
          summary[2].status == 'delivered',
      'sender exact summary after A',
    );
    final resultB = await engines[1].syncConversation(b);
    check(
      resultB.receiptState!.readSeq == Seq.fromWire('1') &&
          resultB.deliveredSeqs.length == 3,
      'B independent delivery plus merged user cursor',
    );
    queues[1].acceptDelivered(resultB, b);
    activities[1].update(
      foreground: false,
      focused: false,
      activeConversation: b.conversationId,
    );
    queues[1].observe(Seq.fromWire('2'), 1, b);
    await Future<void>.delayed(const Duration(milliseconds: 900));
    check((await unread(a)).count == 2, 'background exposure never reads');
    activities[1].update(
      foreground: true,
      focused: true,
      activeConversation: b.conversationId,
    );
    await until(
      () =>
          queues[1].locallyReadSeqs.contains(Seq.fromWire('2')) ||
          channelFailures.isNotEmpty,
      'B foreground read',
    );
    check((await unread(a)).count == 1, 'exact remaining unread');
    queues[1].observe(Seq.fromWire('1'), 1, b);
    await until(
      () =>
          queues[1].locallyReadSeqs.contains(Seq.fromWire('1')) ||
          channelFailures.isNotEmpty,
      'B independently reads same message',
    );
    summary = await summaries();
    check(
      summary[0].visibleReadCount == 1 &&
          summary[1].status == 'read' &&
          summary[2].status == 'delivered',
      'two devices count one recipient',
    );
    final duplicate = ReceiptState.fromJson(
      await request(
        'PUT',
        'read',
        b,
        body: {
          'seqs': ['1', '2'],
        },
      ),
    );
    check(
      duplicate.duplicate && duplicate.readSeq == Seq.fromWire('2'),
      'idempotent read',
    );
    check(channelFailures.isEmpty, 'queues remain healthy');
    stdout.writeln(
      jsonEncode({
        'status': 'passed',
        'messages': 3,
        'bob_devices': 2,
        'remaining_unread': 1,
        'tls_verified': true,
        'websocket_ready': true,
        'websocket_notification': noticeCount > 0,
      }),
    );
  } finally {
    for (final queue in queues) {
      queue.dispose();
    }
    for (final activity in activities) {
      activity.dispose();
    }
    for (final engine in engines) {
      engine.stop();
    }
    await signals?.cancel();
    await failures?.cancel();
    await channel?.dispose();
    for (final store in stores) {
      await store.closeAndErase();
    }
    api.close();
    if (await root.exists()) await root.delete(recursive: true);
  }
}
