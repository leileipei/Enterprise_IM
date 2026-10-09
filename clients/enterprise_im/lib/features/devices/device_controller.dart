import '../../core/auth/client_session.dart';
import '../../core/auth/auth_session.dart';
import '../../core/protocol/api_client.dart';
import '../../core/protocol/models.dart';
import '../../core/protocol/seq.dart';
import '../../core/storage/records.dart';
import '../memberships/membership_controller.dart';

final class OwnDevice {
  final String id, registrationId, platform, name, status;
  final DateTime createdAt, expiresAt;
  final DateTime? revokedAt, lastSeenAt;
  OwnDevice._(
    this.id,
    this.registrationId,
    this.platform,
    this.name,
    this.status,
    this.createdAt,
    this.expiresAt,
    this.revokedAt,
    this.lastSeenAt,
  );
  factory OwnDevice.fromJson(Map<String, Object?> d) {
    final platform = stringField(d, 'platform', max: 16),
        name = stringField(d, 'name', max: 160),
        status = stringField(d, 'status', max: 16);
    final reg = uuidField(d, 'registration_id'),
        created = wireTime(d['created_at']),
        expiry = wireTime(d['expires_at']);
    final revoked = d.containsKey('revoked_at')
        ? wireTime(d['revoked_at'])
        : null;
    if (!const [
          'web',
          'macos',
          'windows',
          'ios',
          'android',
        ].contains(platform) ||
        !const ['active', 'expired', 'revoked'].contains(status) ||
        name.trim() != name ||
        name.runes.length > 80 ||
        RegExp(r'[\x00-\x1f\x7f-\x9f]').hasMatch(name) ||
        reg[14] != '7' ||
        expiry.isBefore(created) ||
        (status == 'revoked') != (revoked != null))
      throw const FormatException('设备信息无效');
    return OwnDevice._(
      uuidField(d, 'device_id'),
      reg,
      platform,
      name,
      status,
      created,
      expiry,
      revoked,
      d.containsKey('last_seen_at') ? wireTime(d['last_seen_at']) : null,
    );
  }
}

final class DevicePageResult {
  final List<OwnDevice> devices;
  final String? cursor;
  DevicePageResult._(this.devices, this.cursor);
  factory DevicePageResult.fromJson(Map<String, Object?> d, String? previous) {
    final raw = d['devices'];
    if (raw is! List || raw.length > 100) throw const FormatException('设备列表无效');
    final ids = <String>{}, registrations = <String>{}, devices = <OwnDevice>[];
    for (final item in raw) {
      final device = OwnDevice.fromJson(objectMap(item));
      if (!ids.add(device.id) || !registrations.add(device.registrationId))
        throw const FormatException('重复设备');
      devices.add(device);
    }
    final more = boolField(d, 'has_more');
    String? cursor;
    if (more) {
      cursor = stringField(d, 'next_cursor', max: 256);
      if (cursor == previous || RegExp(r'[\x00-\x20\x7f]').hasMatch(cursor))
        throw const FormatException('设备游标无效');
    } else if (d.containsKey('next_cursor'))
      throw const FormatException('设备游标无效');
    return DevicePageResult._(List.unmodifiable(devices), cursor);
  }
}

final class ClientCapabilities {
  final int maxActiveDevices;
  ClientCapabilities._(this.maxActiveDevices);
  factory ClientCapabilities.fromJson(Map<String, Object?> d) {
    Seq.fromWire(d['version']);
    wireTime(d['server_time']);
    final limit = d['max_active_devices'],
        lease = d['device_lease_days'],
        cache = d['offline_cache_seconds'];
    if (limit is! int ||
        limit < 1 ||
        limit > 100 ||
        lease is! int ||
        lease < 1 ||
        lease > 90 ||
        cache is! int ||
        cache < 0 ||
        cache > 3600 ||
        !const [
          'owner_admin',
          'sender_aggregate',
        ].contains(d['group_receipt_visibility']) ||
        !boolField(d, 'receipts_enabled'))
      throw const FormatException('设备策略无效');
    return ClientCapabilities._(limit);
  }
}

final class DeviceRevocationResult {
  final bool stoppedOwnClient;
  const DeviceRevocationResult(this.stoppedOwnClient);
}

// Management is account-scoped and works before a device is admitted. Every
// response still belongs to the exact authenticated client captured at opening.
final class DeviceController {
  final ClientSession owner;
  final MembershipController? memberships;
  late final AuthenticatedClient client;
  final _cancellations = <Cancellation>{};
  bool _closed = false, _busy = false;
  bool ownClientStopped = false;
  final _observed = <String, OwnDevice>{};
  String? _nextCursor;
  int _epoch = 0;
  DeviceController(this.owner, {this.memberships}) {
    final current = owner.current;
    if (current == null) throw const AuthFailure('reauthenticate');
    client = current;
    if (memberships != null && !identical(memberships!.owner, owner))
      throw const AuthFailure('identity_changed');
  }
  void _check(int epoch) {
    if (_closed || epoch != _epoch || !identical(owner.current, client))
      throw const ApiFailure('cancelled');
  }

  Future<Map<String, Object?>> _request(
    String method,
    String path, {
    String? role,
    bool noContent = false,
  }) async {
    final epoch = _epoch, cancel = Cancellation();
    _check(epoch);
    _cancellations.add(cancel);
    try {
      final d = await owner.auth.api.json(
        method,
        path,
        bearer: client.session.accessToken,
        actingMembershipId: role,
        cancellation: cancel,
        expectNoContent: noContent,
      );
      _check(epoch);
      return d;
    } finally {
      cancel.cancel();
      _cancellations.remove(cancel);
    }
  }

  Future<DevicePageResult> list({String? cursor}) async {
    final epoch = _epoch;
    _check(epoch);
    if (cursor != null && cursor != _nextCursor)
      throw const ApiFailure('invalid_cursor');
    final page = DevicePageResult.fromJson(
      await _request(
        'GET',
        Uri(
          path: '/api/v1/devices',
          queryParameters: {
            'limit': '100',
            if (cursor != null) 'cursor': cursor,
          },
        ).toString(),
      ),
      cursor,
    );
    if (cursor == null) _observed.clear();
    for (final device in page.devices) {
      if (_observed.containsKey(device.id) ||
          _observed.values.any(
            (old) => old.registrationId == device.registrationId,
          ))
        throw const FormatException('重复设备');
    }
    for (final device in page.devices) {
      _observed[device.id] = device;
    }
    _nextCursor = page.cursor;
    final own = owner.sessions.device?.deviceId;
    if (page.devices.any(
      (device) => device.id == own && device.status == 'revoked',
    )) {
      memberships?.withdrawDevice();
      client.store.invalidateReading();
      await client.store.clearReceivedState();
      _check(epoch);
      await owner.logout();
      ownClientStopped = true;
    }
    return page;
  }

  Future<ClientCapabilities> capabilities(String role) async =>
      ClientCapabilities.fromJson(
        await _request('GET', '/api/v1/me/client-capabilities', role: role),
      );
  Future<PendingDeviceRevocation?> pending() async {
    final epoch = _epoch;
    _check(epoch);
    final value = await client.store.pendingDeviceRevocation();
    _check(epoch);
    return value;
  }

  Future<PendingRegistration?> pendingRegistration() async {
    final epoch = _epoch;
    _check(epoch);
    final value = await client.store.pendingRegistration();
    _check(epoch);
    return value;
  }

  Future<DeviceRevocationResult> revoke(String id) => _start(id, null);
  Future<DeviceRevocationResult> revokeOrphan(OwnDevice device) async {
    final registration = await pendingRegistration();
    if (!identical(_observed[device.id], device) ||
        registration == null ||
        registration.registrationId != device.registrationId ||
        owner.sessions.device?.deviceId == device.id)
      throw const ApiFailure('registration_context_mismatch');
    return _start(device.id, registration.registrationId);
  }

  Future<DeviceRevocationResult> _start(String id, String? registration) async {
    if (_busy) throw const ApiFailure('context_in_progress');
    _busy = true;
    final epoch = _epoch;
    try {
      _check(epoch);
      uuidField({'id': id}, 'id');
      if (await pending() != null)
        throw const ApiFailure('device_revocation_unconfirmed');
      final saved = owner.sessions.device;
      final record = PendingDeviceRevocation(
        deviceId: id,
        wasCurrent: saved?.deviceId == id,
        registrationId: registration,
      );
      await client.store.putPendingDeviceRevocation(record);
      _check(epoch);
      return await _revoke(record, epoch);
    } finally {
      _busy = false;
    }
  }

  Future<DeviceRevocationResult> retry() async {
    if (_busy) throw const ApiFailure('context_in_progress');
    _busy = true;
    final epoch = _epoch;
    try {
      final record = await pending();
      if (record == null) throw const ApiFailure('no_pending_operation');
      return await _revoke(record, epoch);
    } finally {
      _busy = false;
    }
  }

  Future<DeviceRevocationResult> _revoke(
    PendingDeviceRevocation record,
    int epoch,
  ) async {
    _check(epoch);
    final own =
        record.wasCurrent || owner.sessions.device?.deviceId == record.deviceId;
    if (own) {
      memberships?.withdrawDevice();
      client.store.invalidateReading();
      await client.store.clearReceivedState();
      _check(epoch);
    }
    await _request(
      'DELETE',
      '/api/v1/devices/${record.deviceId}',
      noContent: true,
    );
    _check(epoch);
    if (own) {
      await owner.logout();
      ownClientStopped = true;
      return const DeviceRevocationResult(true);
    }
    if (record.registrationId != null) {
      final registration = await client.store.pendingRegistration();
      _check(epoch);
      if (registration != null &&
          registration.registrationId != record.registrationId)
        throw const ApiFailure('registration_context_mismatch');
      await client.store.finishRegistration(record.registrationId!);
      _check(epoch);
    }
    await client.store.finishDeviceRevocation(record);
    _check(epoch);
    return const DeviceRevocationResult(false);
  }

  Future<void> renew() async {
    if (_busy) throw const ApiFailure('context_in_progress');
    _busy = true;
    final epoch = _epoch;
    try {
      _check(epoch);
      if (await pending() != null)
        throw const ApiFailure('device_revocation_unconfirmed');
      final roles = memberships;
      if (roles == null) throw const AuthFailure('invalid_device');
      await roles.renew();
      _check(epoch);
    } finally {
      _busy = false;
    }
  }

  void close() {
    _closed = true;
    _epoch++;
    for (final c in _cancellations) {
      c.cancel();
    }
    _cancellations.clear();
    _observed.clear();
  }
}
