import 'dart:convert';
import 'seq.dart';

Map<String, Object?> objectMap(Object? value) {
  if (value is! Map<String, Object?>) throw const FormatException('响应格式无效');
  return value;
}

String stringField(
  Map<String, Object?> data,
  String key, {
  int max = 8192,
  bool empty = false,
}) {
  final value = data[key];
  if (value is! String || value.length > max || (!empty && value.isEmpty)) {
    throw const FormatException('响应字段无效');
  }
  return value;
}

bool boolField(Map<String, Object?> data, String key) {
  final value = data[key];
  if (value is! bool) throw const FormatException('响应字段无效');
  return value;
}

String uuidField(Map<String, Object?> data, String key) {
  final value = stringField(data, key, max: 36);
  if (!RegExp(
    r'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$',
  ).hasMatch(value)) {
    throw const FormatException('身份编号无效');
  }
  return value;
}

DateTime wireTime(Object? raw) {
  if (raw is! String ||
      !RegExp(
        r'^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?Z$',
      ).hasMatch(raw)) {
    throw const FormatException('响应时间无效');
  }
  final result = DateTime.tryParse(raw);
  if (result == null ||
      !result.isUtc ||
      result.toIso8601String().substring(0, 19) != raw.substring(0, 19)) {
    throw const FormatException('响应时间无效');
  }
  return result;
}

bool proofValid(String proof) {
  if (!RegExp(r'^[A-Za-z0-9_-]{43}$').hasMatch(proof)) return false;
  try {
    final bytes = base64Url.decode(base64Url.normalize(proof));
    return bytes.length == 32 &&
        base64Url.encode(bytes).replaceAll('=', '') == proof;
  } on FormatException {
    return false;
  }
}

String messageBodyField(
  Map<String, Object?> data,
  String key, {
  bool empty = false,
}) {
  final value = stringField(data, key, max: 16384, empty: empty);
  if (utf8.encode(value).length > 16384 || value.contains('\u0000'))
    throw const FormatException('消息正文无效');
  return value;
}

final class DeviceGrant {
  final String deviceId, deviceToken;
  final DateTime expiresAt, serverTime;
  DeviceGrant._(
    this.deviceId,
    this.deviceToken,
    this.expiresAt,
    this.serverTime,
  );
  factory DeviceGrant.fromJson(Map<String, Object?> json) {
    final expiry = wireTime(json['expires_at']);
    final server = wireTime(json['server_time']);
    final token = stringField(json, 'device_token', max: 43);
    if (!proofValid(token) || !expiry.isAfter(server))
      throw const FormatException('设备凭据无效');
    return DeviceGrant._(uuidField(json, 'device_id'), token, expiry, server);
  }
}

final class MessageDTO {
  final Seq seq;
  final bool redacted;
  final String? messageId, senderUserId, messageType, text, caption;
  final Map<String, Object?>? attachment;
  final DateTime? serverTime, contentExpiresAt, cacheUntil, onlineUntil;
  MessageDTO._(
    this.seq,
    this.redacted, {
    this.messageId,
    this.senderUserId,
    this.messageType,
    this.text,
    this.caption,
    this.attachment,
    this.serverTime,
    this.contentExpiresAt,
    this.cacheUntil,
    this.onlineUntil,
  });
  factory MessageDTO.fromJson(Map<String, Object?> json) {
    final seq = Seq.fromWire(json['seq']);
    if (seq.value == BigInt.zero) throw const FormatException('消息编号无效');
    final redacted = json.containsKey('redacted')
        ? boolField(json, 'redacted')
        : false;
    if (redacted) {
      if (json.keys.any((key) => key != 'seq' && key != 'redacted'))
        throw const FormatException('遮蔽消息含正文');
      return MessageDTO._(seq, true);
    }
    final id = uuidField(json, 'message_id');
    final sender = uuidField(json, 'sender_user_id');
    final kind = stringField(json, 'message_type', max: 16);
    final time = wireTime(json['server_time']);
    final expiry = wireTime(json['content_expires_at']);
    final cache = wireTime(json['cache_until']);
    final online = json.containsKey('online_until')
        ? wireTime(json['online_until'])
        : null;
    if (!expiry.isAfter(time) || cache.isAfter(expiry))
      throw const FormatException('消息阅读期限无效');
    String? text, caption;
    Map<String, Object?>? attachment;
    if (kind == 'text') {
      text = messageBodyField(json, 'text');
      if (json.containsKey('attachment') || json.containsKey('caption'))
        throw const FormatException('文本消息格式无效');
    } else if (kind == 'file') {
      if (json.containsKey('text')) throw const FormatException('附件消息格式无效');
      caption = messageBodyField(json, 'caption', empty: true);
      final card = objectMap(json['attachment']);
      uuidField(card, 'file_id');
      final available = boolField(card, 'available');
      final download = boolField(card, 'download_available');
      if (!available &&
          (download ||
              card.keys.any(
                (key) => !const [
                  'file_id',
                  'available',
                  'download_available',
                ].contains(key),
              )))
        throw const FormatException('附件遮蔽格式无效');
      if (available) {
        stringField(card, 'original_filename', max: 1024);
        final size = Seq.fromWire(card['actual_size_bytes']);
        if (size.value == BigInt.zero ||
            size.value > BigInt.from(25 * 1024 * 1024))
          throw const FormatException('附件大小无效');
        if (!const [
          'application/pdf',
          'image/png',
          'image/jpeg',
          'text/plain',
        ].contains(card['detected_media_type']))
          throw const FormatException('附件类型无效');
      }
      attachment = Map<String, Object?>.unmodifiable(card);
    } else {
      throw const FormatException('消息类型无效');
    }
    return MessageDTO._(
      seq,
      false,
      messageId: id,
      senderUserId: sender,
      messageType: kind,
      text: text,
      caption: caption,
      attachment: attachment,
      serverTime: time,
      contentExpiresAt: expiry,
      cacheUntil: cache,
      onlineUntil: online,
    );
  }
}

final class SyncPage {
  final String conversationId;
  final List<MessageDTO> messages;
  final Seq nextAfterSeq, receiptRevision;
  final bool hasMore;
  final String? syncProof;
  final DateTime? proofExpiresAt;
  final DateTime serverTime;
  SyncPage._(
    this.conversationId,
    this.messages,
    this.nextAfterSeq,
    this.receiptRevision,
    this.hasMore,
    this.syncProof,
    this.proofExpiresAt,
    this.serverTime,
  );
  factory SyncPage.fromJson(Map<String, Object?> json, {required Seq after}) {
    final id = uuidField(json, 'conversation_id');
    final raw = json['messages'];
    if (raw is! List<Object?> || raw.length > 500)
      throw const FormatException('同步消息页无效');
    final messages = raw
        .map((item) => MessageDTO.fromJson(objectMap(item)))
        .toList(growable: false);
    var previous = after;
    for (final item in messages) {
      if (item.seq != previous.next) throw const FormatException('同步消息不连续');
      previous = item.seq;
    }
    final next = Seq.fromWire(json['next_after_seq']);
    final revision = Seq.fromWire(json['receipt_revision']);
    final more = boolField(json, 'has_more');
    if (next != previous || (more && messages.isEmpty))
      throw const FormatException('同步页边界无效');
    final server = wireTime(json['server_time']);
    String? proof;
    DateTime? proofExpiry;
    if (messages.isEmpty) {
      if (!json.containsKey('sync_proof') ||
          !json.containsKey('proof_expires_at') ||
          json['sync_proof'] != null ||
          json['proof_expires_at'] != null)
        throw const FormatException('空页证明无效');
    } else {
      proof = stringField(json, 'sync_proof', max: 43);
      proofExpiry = wireTime(json['proof_expires_at']);
      if (!proofValid(proof) ||
          !proofExpiry.isAfter(server) ||
          proofExpiry.isAfter(server.add(const Duration(seconds: 300))))
        throw const FormatException('同步证明无效');
    }
    for (final item in messages) {
      if (item.redacted) continue;
      if (item.serverTime!.isAfter(server) ||
          !item.contentExpiresAt!.isAfter(server) ||
          item.cacheUntil!.isBefore(server))
        throw const FormatException('阅读期限已失效');
      final online = item.onlineUntil;
      if (online != null &&
          (item.cacheUntil != server ||
              !online.isAfter(server) ||
              online.isAfter(server.add(const Duration(seconds: 5))) ||
              online.isAfter(item.contentExpiresAt!)))
        throw const FormatException('在线阅读授权无效');
    }
    return SyncPage._(
      id,
      List<MessageDTO>.unmodifiable(messages),
      next,
      revision,
      more,
      proof,
      proofExpiry,
      server,
    );
  }
}

final class MessageACK {
  final String messageId, conversationId;
  final Seq seq;
  final DateTime serverTime;
  final bool duplicate;
  MessageACK._(
    this.messageId,
    this.conversationId,
    this.seq,
    this.serverTime,
    this.duplicate,
  );
  factory MessageACK.fromJson(Map<String, Object?> json) {
    final seq = Seq.fromWire(json['seq'], legacyInteger: true);
    if (seq.value == BigInt.zero) throw const FormatException('消息ACK无效');
    return MessageACK._(
      uuidField(json, 'message_id'),
      uuidField(json, 'conversation_id'),
      seq,
      wireTime(json['server_time']),
      boolField(json, 'duplicate'),
    );
  }
}

final class ConversationSummary {
  final String id, kind, name;
  final Seq lastSeq;
  ConversationSummary._(this.id, this.kind, this.name, this.lastSeq);
  factory ConversationSummary.fromJson(Map<String, Object?> json) {
    final kind = stringField(json, 'type', max: 16);
    if (kind != 'direct' && kind != 'group')
      throw const FormatException('会话类型无效');
    final name = kind == 'group'
        ? stringField(json, 'name', max: 1024)
        : boolField(json, 'peer_visible')
        ? stringField(json, 'display_name', max: 1024)
        : '不可见联系人';
    return ConversationSummary._(
      uuidField(json, 'id'),
      kind,
      name,
      Seq.fromWire(json['last_seq'], legacyInteger: true),
    );
  }
}

final class ReceiptState {
  final Seq scannedSeq, deliveredSeq, readSeq, receiptRevision;
  final bool duplicate;
  ReceiptState._(
    this.scannedSeq,
    this.deliveredSeq,
    this.readSeq,
    this.receiptRevision,
    this.duplicate,
  );
  factory ReceiptState.fromJson(Map<String, Object?> value) {
    final scanned = Seq.fromWire(value['scanned_seq']),
        delivered = Seq.fromWire(value['delivered_seq']);
    if (delivered.compareTo(scanned) > 0) throw const FormatException('送达游标无效');
    // User read may be ahead of this device's scan. It never grants this
    // device per-message delivery or read authority.
    return ReceiptState._(
      scanned,
      delivered,
      Seq.fromWire(value['read_seq']),
      Seq.fromWire(value['receipt_revision']),
      boolField(value, 'duplicate'),
    );
  }
}

int _receiptCount(Map<String, Object?> data, String key) {
  final value = data[key];
  if (value is! int || value < 0 || BigInt.from(value) > Seq.maximum) {
    throw const FormatException('回执数量无效');
  }
  return value;
}

final class UnreadState {
  final int count;
  final bool exact;
  final Seq headSeq, receiptRevision, policyVersion;
  UnreadState._(
    this.count,
    this.exact,
    this.headSeq,
    this.receiptRevision,
    this.policyVersion,
  );
  factory UnreadState.fromJson(Map<String, Object?> data) {
    final count = _receiptCount(data, 'unread_count');
    final head = Seq.fromWire(data['head_seq']);
    if (BigInt.from(count) > head.value) throw const FormatException('未读数量越界');
    return UnreadState._(
      count,
      boolField(data, 'exact'),
      head,
      Seq.fromWire(data['receipt_revision']),
      Seq.fromWire(data['policy_version']),
    );
  }
}

final class ReceiptSummary {
  final Seq seq;
  final String status;
  final bool summaryAvailable;
  final int visibleRecipientCount, visibleDeliveredCount, visibleReadCount;
  final DateTime? deliveredAt, readAt;
  ReceiptSummary._(
    this.seq,
    this.status,
    this.summaryAvailable,
    this.visibleRecipientCount,
    this.visibleDeliveredCount,
    this.visibleReadCount,
    this.deliveredAt,
    this.readAt,
  );
  factory ReceiptSummary.fromJson(Map<String, Object?> data) {
    final seq = Seq.fromWire(data['seq']);
    final status = stringField(data, 'status', max: 9);
    final available = boolField(data, 'summary_available');
    final recipients = _receiptCount(data, 'visible_recipient_count');
    final delivered = _receiptCount(data, 'visible_delivered_count');
    final read = _receiptCount(data, 'visible_read_count');
    final deliveredAt = data['delivered_at'] == null
        ? null
        : wireTime(data['delivered_at']);
    final readAt = data['read_at'] == null ? null : wireTime(data['read_at']);
    final expected = recipients > 0 && read == recipients
        ? 'read'
        : recipients > 0 && delivered == recipients
        ? 'delivered'
        : 'sent';
    if (seq.value == BigInt.zero ||
        read > delivered ||
        delivered > recipients ||
        status != expected ||
        (!available &&
            (recipients != 0 || deliveredAt != null || readAt != null)) ||
        (deliveredAt != null && delivered == 0) ||
        (readAt != null && read == 0) ||
        (readAt != null && deliveredAt == null) ||
        (readAt != null &&
            deliveredAt != null &&
            readAt.isBefore(deliveredAt))) {
      throw const FormatException('回执摘要无效');
    }
    return ReceiptSummary._(
      seq,
      status,
      available,
      recipients,
      delivered,
      read,
      deliveredAt,
      readAt,
    );
  }
}
