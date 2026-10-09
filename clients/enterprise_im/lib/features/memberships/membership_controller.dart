import '../../core/auth/client_session.dart';
import '../../core/auth/auth_session.dart';
import '../../core/protocol/api_client.dart';
import '../../core/protocol/models.dart';
import '../../core/protocol/client_id.dart';
import '../../core/storage/records.dart';
import 'membership_model.dart';
export 'membership_model.dart';

final class WorkspaceAdmission {
  final AuthenticatedClient client;
  final Membership membership;
  final DeviceGrant device;
  final ServerClock clock;
  final Duration _deadline;
  WorkspaceAdmission._(
    this.client,
    this.membership,
    this.device,
    this.clock,
    this._deadline,
  );
  Duration get remaining {
    final device = _deadline - LeaseClock.monotonicNow();
    final session = client.session.remaining;
    final value = device < session ? device : session;
    return value.isNegative ? Duration.zero : value;
  }

  bool get isExpired => remaining == Duration.zero;
  @override
  String toString() => 'WorkspaceAdmission';
}

// Authentication admits the account repository; this controller separately
// admits an explicitly selected membership and a freshly verified device.
final class MembershipController {
  final ClientSession owner;
  List<Membership> _memberships = const [];
  WorkspaceAdmission? _current;
  AuthenticatedClient? _loadedClient;
  Cancellation? _cancel;
  int _epoch = 0;
  bool _closed = false, _busy = false;
  MembershipController(this.owner);
  List<Membership> get memberships =>
      !_closed &&
          _loadedClient != null &&
          identical(owner.current, _loadedClient)
      ? _memberships
      : const [];
  WorkspaceAdmission? get current {
    final value = _current;
    return !_closed &&
            value != null &&
            identical(owner.current, value.client) &&
            !value.isExpired
        ? value
        : null;
  }

  AuthenticatedClient _client() {
    final client = owner.current;
    if (_closed || client == null) throw const AuthFailure('reauthenticate');
    if (client.session.origin != owner.auth.api.origin)
      throw const AuthFailure('identity_changed');
    return client;
  }

  void _check(int epoch, AuthenticatedClient client) {
    if (_closed ||
        epoch != _epoch ||
        !identical(owner.current, client) ||
        client.session.isExpired)
      throw const AuthCancelled();
  }

  Future<List<Membership>> load() async {
    if (_busy) throw const AuthFailure('context_in_progress');
    final client = _client();
    _current = null;
    _memberships = const [];
    _loadedClient = null;
    _busy = true;
    final epoch = ++_epoch, cancel = Cancellation();
    _cancel = cancel;
    try {
      final data = await owner.auth.api.json(
        'GET',
        '/api/v1/me',
        bearer: client.session.accessToken,
        cancellation: cancel,
      );
      _check(epoch, client);
      final self = SelfContext.fromJson(data);
      if (self.tenantId != client.session.tenantId ||
          self.userId != client.session.userId)
        throw const AuthFailure('identity_changed');
      _loadedClient = client;
      return _memberships = self.memberships;
    } finally {
      cancel.cancel();
      if (epoch == _epoch) {
        _cancel = null;
        _busy = false;
      }
    }
  }

  Future<WorkspaceAdmission> select(String membershipId) async {
    if (_busy) throw const AuthFailure('context_in_progress');
    final client = _client(), ready = current;
    if (!identical(client, _loadedClient))
      throw const AuthFailure('reauthenticate');
    if (ready != null) {
      if (ready.membership.id == membershipId) return ready;
      throw const AuthFailure('membership_switch_requires_reauthentication');
    }
    final found = _memberships.where((m) => m.id == membershipId).toList();
    if (found.length != 1) throw const AuthFailure('invalid_acting_membership');
    _current = null;
    _busy = true;
    final epoch = ++_epoch, cancel = Cancellation();
    _cancel = cancel;
    try {
      if (await client.store.pendingDeviceRevocation() != null)
        throw const AuthFailure('device_revocation_unconfirmed');
      final sends = await client.store.pendingSends();
      final action = await client.store.pendingAction();
      final saved = owner.sessions.device;
      var pending = await client.store.pendingRegistration();
      _check(epoch, client);
      if (sends.any((p) => p.actingMembershipId != membershipId))
        throw const AuthFailure('send_context_mismatch');
      if (action != null && action.actingMembershipId != membershipId)
        throw const AuthFailure('operation_context_mismatch');
      if (pending != null && pending.actingMembershipId != membershipId)
        throw const AuthFailure('registration_context_mismatch');
      owner.claimMembership(client, membershipId);
      DeviceGrant device;
      ServerClock clock;
      Duration deadline;
      if (saved != null) {
        final started = LeaseClock.monotonicNow();
        final data = await owner.auth.api.json(
          'POST',
          '/api/v1/devices/${saved.deviceId}/renew',
          bearer: client.session.accessToken,
          actingMembershipId: membershipId,
          device: saved,
          cancellation: cancel,
        );
        _check(epoch, client);
        device = renewedDevice(data, saved);
        clock = ServerClock.start(device.serverTime, started);
        deadline = started + device.expiresAt.difference(device.serverTime);
      } else {
        if (pending != null &&
            (pending.actingMembershipId != membershipId ||
                pending.platform != owner.platform))
          throw const AuthFailure('registration_context_mismatch');
        if (pending == null) {
          final started = LeaseClock.monotonicNow();
          final data = await owner.auth.api.json(
            'GET',
            '/api/v1/native/config?platform=${owner.platform}',
            cancellation: cancel,
          );
          _check(epoch, client);
          if (stringField(data, 'platform', max: 16) != owner.platform)
            throw const AuthFailure('invalid_configuration');
          clock = ServerClock.start(wireTime(data['server_time']), started);
          pending = PendingRegistration(
            registrationId: ClientMessageId.create(clock),
            actingMembershipId: membershipId,
            platform: owner.platform,
            name: '企业 IM ${owner.platform}',
          );
          await client.store.putPendingRegistration(pending);
          _check(epoch, client);
        }
        final started = LeaseClock.monotonicNow();
        final data = await owner.auth.api.json(
          'POST',
          '/api/v1/devices',
          body: pending.body,
          bearer: client.session.accessToken,
          actingMembershipId: membershipId,
          cancellation: cancel,
        );
        _check(epoch, client);
        device = DeviceGrant.fromJson(data);
        clock = ServerClock.start(device.serverTime, started);
        deadline = started + device.expiresAt.difference(device.serverTime);
      }
      if (LeaseClock.monotonicNow() >= deadline)
        throw const AuthFailure('device_expired');
      await owner.sessions.saveCredentials(
        client.session,
        refreshToken: owner.sessions.refreshToken,
        device: device,
      );
      _check(epoch, client);
      if (pending != null) {
        await client.store.finishRegistration(pending.registrationId);
        _check(epoch, client);
      }
      final admission = WorkspaceAdmission._(
        client,
        found.single,
        device,
        clock,
        deadline,
      );
      if (admission.isExpired) throw const AuthFailure('device_expired');
      return _current = admission;
    } finally {
      cancel.cancel();
      if (epoch == _epoch) {
        _cancel = null;
        _busy = false;
      }
    }
  }

  void withdrawDevice() {
    _epoch++;
    _cancel?.cancel();
    _cancel = null;
    _current = null;
    _busy = false;
    owner.current?.store.invalidateReading();
  }

  Future<WorkspaceAdmission> renew() {
    final ready = current;
    if (ready == null ||
        _busy ||
        owner.sessions.device?.deviceId != ready.device.deviceId)
      throw const AuthFailure('invalid_device');
    withdrawDevice();
    return select(ready.membership.id);
  }

  void close() {
    _closed = true;
    _epoch++;
    _cancel?.cancel();
    _cancel = null;
    _current = null;
    _memberships = const [];
    _loadedClient = null;
  }
}
