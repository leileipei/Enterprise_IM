import 'dart:async';
import 'package:flutter/material.dart';
import '../../core/auth/client_session.dart';
import '../../core/auth/auth_session.dart';
import '../../core/protocol/api_client.dart';
import '../../core/storage/records.dart';
import '../memberships/membership_controller.dart';
import 'device_controller.dart';

class DevicePage extends StatefulWidget {
  final ClientSession owner;
  final MembershipController memberships;
  final String? actingMembershipId;
  final Future<bool> Function() onRetryLogoutCleanup;
  const DevicePage({
    super.key,
    required this.owner,
    required this.memberships,
    this.actingMembershipId,
    required this.onRetryLogoutCleanup,
  });
  @override
  State<DevicePage> createState() => _DevicePageState();
}

class _DevicePageState extends State<DevicePage> {
  late final DeviceController _controller;
  List<OwnDevice> _devices = const [];
  ClientCapabilities? _policy;
  PendingRegistration? _registration;
  PendingDeviceRevocation? _pending;
  String? _cursor, _notice;
  bool _busy = false, _expired = false, _cleanupFailed = false;
  Timer? _expiry;
  int _epoch = 0;
  bool get _active =>
      mounted &&
      !_expired &&
      identical(widget.owner.current, _controller.client);
  @override
  void initState() {
    super.initState();
    _controller = DeviceController(
      widget.owner,
      memberships: widget.memberships,
    );
    _expiry = Timer(_controller.client.session.remaining, () {
      _controller.close();
      _epoch++;
      if (mounted)
        setState(() {
          _expired = true;
          _busy = false;
          _devices = const [];
          _registration = null;
          _pending = null;
          _policy = null;
          _cursor = null;
          _notice = '登录已到期，请返回登录页。';
        });
    });
    unawaited(_run(() => _load()));
  }

  Future<void> _load({bool more = false}) async {
    final page = await _controller.list(cursor: more ? _cursor : null);
    if (_controller.ownClientStopped) {
      if (mounted) Navigator.pop(context, true);
      return;
    }
    if (!_active) return;
    _devices = List.unmodifiable([if (more) ..._devices, ...page.devices]);
    _cursor = page.cursor;
    _registration = await _controller.pendingRegistration();
    _pending = await _controller.pending();
    if (widget.actingMembershipId != null) {
      try {
        _policy = await _controller.capabilities(widget.actingMembershipId!);
      } catch (_) {
        _policy = null;
        if (_active) _notice = '设备上限读取失败，仍可管理本人设备。';
      }
    }
  }

  Future<void> _run(Future<void> Function() work) async {
    if (_busy || !_active) return;
    final epoch = ++_epoch;
    setState(() {
      _busy = true;
      _notice = null;
    });
    try {
      await work();
    } catch (error) {
      if (mounted && epoch == _epoch) {
        if (widget.owner.current == null) {
          _devices = const [];
          _cleanupFailed = true;
          _notice = '本机已停止，退出清理未完成，请重试清理。';
        } else {
          _notice = _message(error);
          try {
            _pending = await _controller.pending();
          } catch (_) {
            _devices = const [];
          }
        }
      }
    } finally {
      if (mounted && epoch == _epoch) setState(() => _busy = false);
    }
  }

  bool _own(OwnDevice d) => widget.owner.sessions.device?.deviceId == d.id;
  bool _orphan(OwnDevice d) =>
      !_own(d) && _registration?.registrationId == d.registrationId;
  Future<void> _revoke(OwnDevice d) async {
    if (_busy || !_active || _pending != null) return;
    final accepted = await showDialog<bool>(
      context: context,
      builder: (context) => AlertDialog(
        title: const Text('撤销设备'),
        content: Text(
          _own(d)
              ? '撤销本机后将退出登录，清除本机缓存和待确认消息。是否继续？'
              : '撤销“${d.name}”后，该设备将不能继续使用原凭据。',
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.pop(context, false),
            child: const Text('取消'),
          ),
          FilledButton(
            onPressed: () => Navigator.pop(context, true),
            child: const Text('确认撤销'),
          ),
        ],
      ),
    );
    if (accepted != true || !_active) return;
    await _run(() async {
      final result = await (_orphan(d)
          ? _controller.revokeOrphan(d)
          : _controller.revoke(d.id));
      await _afterRevoke(result);
    });
  }

  Future<void> _afterRevoke(DeviceRevocationResult result) async {
    if (result.stoppedOwnClient) {
      if (mounted) Navigator.pop(context, true);
      return;
    }
    if (!_active) return;
    // A refresh failure must not undo the confirmed original DELETE.
    _pending = null;
    _registration = await _controller.pendingRegistration();
    try {
      await _load();
      if (_active) _notice = '撤销已确认';
    } catch (_) {
      if (_active) {
        _devices = const [];
        _cursor = null;
        _notice = '撤销已确认，设备列表刷新失败，请刷新核实。';
      }
    }
  }

  Future<void> _cleanup() async {
    if (_busy) return;
    setState(() => _busy = true);
    try {
      if (await widget.onRetryLogoutCleanup()) {
        if (mounted) Navigator.pop(context, true);
      }
    } catch (_) {
      /* Keep the explicit cleanup retry available. */
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  static String _message(Object error) {
    final code = error is ApiFailure
        ? error.code
        : error is AuthFailure
        ? error.code
        : null;
    return switch (code) {
      'timeout' || 'unavailable' => '撤销结果尚未确认，请重试原撤销；列表读取失败时可刷新。',
      'device_revocation_unconfirmed' => '请先重试原撤销。',
      'registration_context_mismatch' => '设备与原登记不匹配，请重新读取设备列表。',
      'invalid_identity' || 'reauthenticate' => '登录或任职已失效，请重新登录。',
      'not_found' => '服务端未确认该设备的撤销，请核实后重试原撤销。',
      'rate_limited' => '请求过于频繁，请稍后重试。',
      _ => '操作未确认，请检查连接后重试。',
    };
  }

  @override
  void dispose() {
    _epoch++;
    _expiry?.cancel();
    _controller.close();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final enabled = _active && !_busy && _pending == null;
    return Scaffold(
      appBar: AppBar(
        title: const Text('本人设备'),
        leading: IconButton(
          tooltip: '返回工作空间',
          icon: const Icon(Icons.arrow_back),
          onPressed: _busy ? null : () => Navigator.pop(context),
        ),
      ),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(16),
          children: [
            if (_busy) const LinearProgressIndicator(),
            if (_notice != null)
              Padding(
                padding: const EdgeInsets.symmetric(vertical: 12),
                child: Semantics(liveRegion: true, child: Text(_notice!)),
              ),
            if (_cleanupFailed)
              FilledButton(
                onPressed: _busy ? null : _cleanup,
                child: const Text('重试退出清理'),
              ),
            if (_active) ...[
              Text(
                _policy == null
                    ? '设备上限：未读取（需有效任职）'
                    : '设备上限：${_policy!.maxActiveDevices}',
              ),
              TextButton(
                onPressed: _busy ? null : () => _run(() => _load()),
                child: const Text('刷新设备列表'),
              ),
              if (_pending != null) ...[
                Text('撤销尚未确认：${_pending!.deviceId}'),
                FilledButton(
                  onPressed: _busy
                      ? null
                      : () => _run(
                          () async => _afterRevoke(await _controller.retry()),
                        ),
                  child: const Text('重试原撤销'),
                ),
              ],
              if (widget.memberships.current != null)
                TextButton(
                  onPressed: enabled
                      ? () => _run(() async {
                          await _controller.renew();
                          if (_active) _notice = '本机设备已重新验证';
                        })
                      : null,
                  child: const Text('重新验证本机设备'),
                ),
              for (final d in _devices)
                Card(
                  child: Padding(
                    padding: const EdgeInsets.all(16),
                    child: Column(
                      crossAxisAlignment: CrossAxisAlignment.start,
                      children: [
                        Text(
                          d.name,
                          style: Theme.of(context).textTheme.titleMedium,
                        ),
                        if (_own(d)) const Text('本机'),
                        if (_orphan(d)) const Text('孤立登记（本机凭据未保存）'),
                        Text(
                          '${d.platform} · ${switch (d.status) {
                            'active' => '有效',
                            'expired' => '已到期',
                            _ => '已撤销',
                          }}',
                        ),
                        Text('登记：${d.createdAt.toLocal()}'),
                        Text('租约到期：${d.expiresAt.toLocal()}'),
                        if (d.lastSeenAt != null)
                          Text('最近活动：${d.lastSeenAt!.toLocal()}'),
                        TextButton(
                          key: ValueKey('revoke-device-${d.id}'),
                          onPressed:
                              enabled && (d.status != 'revoked' || _orphan(d))
                              ? () => _revoke(d)
                              : null,
                          child: const Text('撤销设备'),
                        ),
                      ],
                    ),
                  ),
                ),
              if (_cursor != null)
                TextButton(
                  onPressed: _busy ? null : () => _run(() => _load(more: true)),
                  child: const Text('加载更多设备'),
                ),
              if (!_busy && _devices.isEmpty) const Text('没有可显示的设备'),
            ],
          ],
        ),
      ),
    );
  }
}
