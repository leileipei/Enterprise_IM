import '../protocol/models.dart';
import '../protocol/seq.dart';

// A reading grant exists only in this process. Persisted cache_until alone is
// never authority to read after restart. The caller must bind a fresh server
// response and capture its request-start monotonic time to deduct network delay.
final class LeaseClock {
  static final Stopwatch _processClock = Stopwatch()..start();
  static Duration monotonicNow() => _processClock.elapsed;
  final Duration Function() _now;
  final Duration _baseline, _budget;
  final DateTime? serverTime;
  Duration _last;
  bool _authorized;
  LeaseClock._(
    this._now,
    this._baseline,
    this._budget,
    this._authorized,
    this.serverTime,
  ) : _last = _baseline;
  factory LeaseClock.start(
    DateTime serverTime,
    Duration receivedMonotonic,
    DateTime cacheUntil, {
    Duration Function()? now,
  }) {
    final budget = cacheUntil.difference(serverTime);
    if (!serverTime.isUtc ||
        !cacheUntil.isUtc ||
        receivedMonotonic.isNegative ||
        budget.isNegative ||
        budget > const Duration(seconds: 3600)) {
      throw const FormatException('阅读租约基线无效');
    }
    return LeaseClock._(
      now ?? monotonicNow,
      receivedMonotonic,
      budget,
      true,
      serverTime,
    );
  }
  factory LeaseClock.unauthorized() =>
      LeaseClock._(monotonicNow, Duration.zero, Duration.zero, false, null);
  Duration get remaining {
    if (!_authorized) return Duration.zero;
    final current = _now();
    if (current < _last || current < _baseline) {
      invalidate();
      return Duration.zero;
    }
    _last = current;
    final value = _budget - (current - _baseline);
    if (value <= Duration.zero) {
      invalidate();
      return Duration.zero;
    }
    return value;
  }

  Duration remainingFor(DateTime deadline) {
    final available = remaining;
    if (available == Duration.zero || serverTime == null) return Duration.zero;
    final until = deadline.difference(serverTime!);
    final elapsed = _budget - available;
    final bounded = until - elapsed;
    if (bounded <= Duration.zero) return Duration.zero;
    return bounded < available ? bounded : available;
  }

  void invalidate() {
    _authorized = false;
  }
}

final class PendingReceipt {
  final String conversationId, syncProof;
  final Seq nextAfterSeq;
  final DateTime proofExpiresAt;
  final int generation;
  PendingReceipt({
    required this.conversationId,
    required this.syncProof,
    required this.nextAfterSeq,
    required this.proofExpiresAt,
    required this.generation,
  }) {
    uuidField({'id': conversationId}, 'id');
    if (!proofValid(syncProof) || !proofExpiresAt.isUtc || generation < 0) {
      throw const FormatException('待提交回执无效');
    }
  }
  Map<String, Object?> toJson() => {
    'conversation_id': conversationId,
    'sync_proof': syncProof,
    'next_after_seq': nextAfterSeq.toString(),
    'proof_expires_at': proofExpiresAt.toIso8601String(),
    'generation': generation,
  };
  factory PendingReceipt.fromJson(Map<String, Object?> value) => PendingReceipt(
    conversationId: uuidField(value, 'conversation_id'),
    syncProof: stringField(value, 'sync_proof'),
    nextAfterSeq: Seq.fromWire(value['next_after_seq']),
    proofExpiresAt: wireTime(value['proof_expires_at']),
    generation: value['generation']! as int,
  );
}

Map<String, Object?> messageContent(MessageDTO message) => {
  'seq': message.seq.toString(),
  'message_id': message.messageId,
  'sender_user_id': message.senderUserId,
  'message_type': message.messageType,
  if (message.text != null) 'text': message.text,
  if (message.caption != null) 'caption': message.caption,
  if (message.attachment != null) 'attachment': message.attachment,
  'server_time': message.serverTime!.toIso8601String(),
  'content_expires_at': message.contentExpiresAt!.toIso8601String(),
  'cache_until': message.cacheUntil!.toIso8601String(),
};

final class StoredMessage {
  final String conversationId, actingMembershipId;
  final int generation;
  final DateTime authorizationServerTime;
  final MessageDTO message;
  Map<String, Object?> get content => Map.unmodifiable(messageContent(message));
  Seq get seq => message.seq;
  StoredMessage._(
    this.conversationId,
    this.actingMembershipId,
    this.generation,
    this.authorizationServerTime,
    this.message,
  );
  factory StoredMessage.fromJson(Map<String, Object?> value) {
    final message = MessageDTO.fromJson(objectMap(value['content']));
    final server = wireTime(value['authorization_server_time']);
    final generation = value['generation'];
    if (message.redacted ||
        generation is! int ||
        generation < 0 ||
        message.cacheUntil!.isBefore(server) ||
        !message.contentExpiresAt!.isAfter(server)) {
      throw const FormatException('本地消息无效');
    }
    return StoredMessage._(
      uuidField(value, 'conversation_id'),
      uuidField(value, 'acting_membership_id'),
      generation,
      server,
      message,
    );
  }
}

final class PendingSend {
  final String clientMsgId, conversationId, actingMembershipId, text, state;
  final int generation;
  final DateTime createdAt;
  PendingSend({
    required this.clientMsgId,
    required this.conversationId,
    required this.actingMembershipId,
    required this.text,
    required this.generation,
    required this.createdAt,
    this.state = 'pending',
  }) {
    uuidField({'id': conversationId}, 'id');
    uuidField({'id': actingMembershipId}, 'id');
    if (!RegExp(
          r'^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$',
        ).hasMatch(clientMsgId) ||
        generation < 0 ||
        !createdAt.isUtc ||
        state != 'pending') {
      throw const FormatException('待发送记录无效');
    }
    messageBodyField({'text': text}, 'text');
  }
  Map<String, Object?> toJson() => {
    'client_msg_id': clientMsgId,
    'conversation_id': conversationId,
    'acting_membership_id': actingMembershipId,
    'text': text,
    'generation': generation,
    'created_at': createdAt.toIso8601String(),
    'state': state,
  };
  factory PendingSend.fromJson(Map<String, Object?> value) {
    final generation = value['generation'];
    if (generation is! int) throw const FormatException('待发送记录无效');
    return PendingSend(
      clientMsgId: stringField(value, 'client_msg_id', max: 36),
      conversationId: uuidField(value, 'conversation_id'),
      actingMembershipId: uuidField(value, 'acting_membership_id'),
      text: messageBodyField(value, 'text'),
      generation: generation,
      createdAt: wireTime(value['created_at']),
      state: stringField(value, 'state', max: 16),
    );
  }
}

// One unresolved device registration per account. It is encrypted in the same
// database as pending sends, and never contains a device or access token.
final class PendingRegistration {
  final String registrationId, actingMembershipId, platform, name;
  PendingRegistration({
    required this.registrationId,
    required this.actingMembershipId,
    required this.platform,
    required this.name,
  }) {
    uuidField({'id': registrationId}, 'id');
    uuidField({'id': actingMembershipId}, 'id');
    if (registrationId[14] != '7' ||
        !'89ab'.contains(registrationId[19]) ||
        !const ['macos', 'windows', 'android', 'ios'].contains(platform) ||
        name.isEmpty ||
        name.runes.length > 80 ||
        name.trim() != name ||
        RegExp(r'[\x00-\x1f\x7f]').hasMatch(name))
      throw const FormatException('设备登记请求无效');
  }
  Map<String, Object?> get body => {
    'registration_id': registrationId,
    'platform': platform,
    'name': name,
  };
  Map<String, Object?> toJson() => {
    ...body,
    'acting_membership_id': actingMembershipId,
  };
  factory PendingRegistration.fromJson(Map<String, Object?> data) =>
      PendingRegistration(
        registrationId: uuidField(data, 'registration_id'),
        actingMembershipId: uuidField(data, 'acting_membership_id'),
        platform: stringField(data, 'platform', max: 16),
        name: stringField(data, 'name', max: 160),
      );
}

// Confirmation keeps only the ACK and scope metadata, never the message body.
final class SendConfirmation {
  final String clientMsgId, actingMembershipId;
  final MessageACK ack;
  SendConfirmation(this.clientMsgId, this.actingMembershipId, this.ack) {
    uuidField({'id': clientMsgId}, 'id');
    uuidField({'id': actingMembershipId}, 'id');
  }
  Map<String, Object?> toJson() => {
    'client_msg_id': clientMsgId,
    'acting_membership_id': actingMembershipId,
    'ack': {
      'message_id': ack.messageId,
      'conversation_id': ack.conversationId,
      'seq': ack.seq.toString(),
      'server_time': ack.serverTime.toIso8601String(),
      'duplicate': ack.duplicate,
    },
  };
  factory SendConfirmation.fromJson(Map<String, Object?> data) =>
      SendConfirmation(
        uuidField(data, 'client_msg_id'),
        uuidField(data, 'acting_membership_id'),
        MessageACK.fromJson(objectMap(data['ack'])),
      );
}
