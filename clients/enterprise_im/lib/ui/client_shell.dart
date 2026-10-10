import 'dart:async';
import 'package:flutter/material.dart';
import '../core/auth/client_session.dart';
import '../platform/app_activity.dart';
import '../platform/native_activity.dart';
import '../platform/notifications.dart';
import '../core/sync/client_context.dart';
import '../features/conversations/conversation_view.dart';
import 'message_page.dart';
import 'directory_page.dart';
import 'group_page.dart';
import '../features/devices/device_page.dart';
import '../core/auth/auth_session.dart';
import '../core/protocol/api_client.dart';
import '../core/protocol/models.dart';
import '../core/storage/secure_vault.dart';
import '../features/memberships/membership_controller.dart';
import '../features/conversations/conversation_list.dart';

class ClientShell extends StatefulWidget {
  final ClientSession owner;
  final Future<bool> Function() onLogout;
  final AppActivity? activity;
  final NotificationPort? notificationPort;
  final ValueChanged<AuthSession?>? onSessionChanged;
  const ClientShell({
    super.key,
    required this.owner,
    required this.onLogout,
    this.activity,
    this.notificationPort,
    this.onSessionChanged,
  });
  @override
  State<ClientShell> createState() => _ClientShellState();
}

class _ClientShellState extends State<ClientShell> {
  late MembershipController _memberships;
  late ConversationList _lists;
  WorkspaceAdmission? _ready;
  NativeActivityAdapter? _native;
  late final AppActivity _activity;
  late final MessageNotifications _notifications;
  bool _requestingNotifications = false;
  ConversationView? _view;
  ConversationSummary? _detail;
  bool _opening = false;
  List<Membership> _roles = const [];
  List<ConversationSummary> _items = const [];
  Map<String, String> _statuses = const {};
  bool _busy = true, _expired = false, _logoutFailed = false;
  String _kind = 'direct';
  String? _notice, _cursor, _attemptedMembership;
  Timer? _expiry;
  int _epoch = 0;
  @override
  void initState() {
    super.initState();
    if (widget.activity == null) _native = NativeActivityAdapter();
    _activity = widget.activity ?? _native!.activity;
    _notifications = MessageNotifications(
      _activity,
      widget.notificationPort ?? NativeNotificationPort(widget.owner.platform),
    );
    _memberships = MembershipController(widget.owner);
    _lists = ConversationList(_memberships);
    _armExpiry();
    unawaited(_loadRoles());
  }

  void _armExpiry() {
    _expiry?.cancel();
    final session = widget.owner.current?.session;
    if (session == null) {
      _expired = true;
      return;
    }
    _expiry = Timer(session.remaining, () {
      unawaited(_closeDetail());
      _memberships.close();
      _lists.close();
      _epoch++;
      if (mounted)
        setState(() {
          _ready = null;
          _roles = const [];
          _items = const [];
          _expired = true;
          _busy = false;
          _notice = '登录已到期，请返回登录页恢复登录。';
        });
    });
  }

  bool _active(int epoch) => mounted && epoch == _epoch && !_expired;
  Future<void> _loadRoles() async {
    final epoch = ++_epoch;
    setState(() {
      _busy = true;
      _notice = null;
    });
    try {
      final roles = await _memberships.load();
      if (_active(epoch)) setState(() => _roles = roles);
    } catch (error) {
      if (_active(epoch)) setState(() => _notice = _message(error));
    } finally {
      if (_active(epoch)) setState(() => _busy = false);
    }
  }

  Future<void> _select(String id) async {
    _attemptedMembership = id;
    final epoch = ++_epoch;
    setState(() {
      _busy = true;
      _notice = null;
    });
    try {
      final ready = await _memberships.select(id);
      if (!_active(epoch)) return;
      setState(() => _ready = ready);
      final page = await _lists.load(_kind);
      if (_active(epoch)) _page(page, false);
    } catch (error) {
      if (_active(epoch)) setState(() => _notice = _message(error));
    } finally {
      if (_active(epoch)) setState(() => _busy = false);
    }
  }

  void _page(ConversationPage page, bool append) {
    setState(() {
      final records = {
        if (append)
          for (final item in _items) item.id: item,
        for (final item in page.items) item.id: item,
      };
      _items = List.unmodifiable(records.values);
      _statuses = {if (append) ..._statuses, ...page.groupStatuses};
      _cursor = page.nextCursor;
    });
  }

  Future<void> _loadList({String? kind, bool more = false}) async {
    if (_busy || _expired) return;
    final epoch = ++_epoch;
    setState(() {
      _busy = true;
      _notice = null;
      if (kind != null) {
        _kind = kind;
        _items = const [];
        _statuses = const {};
        _cursor = null;
      }
    });
    try {
      final page = await _lists.load(_kind, cursor: more ? _cursor : null);
      if (_active(epoch)) _page(page, more);
    } catch (error) {
      if (_active(epoch))
        setState(() {
          if (_memberships.current == null ||
              error is ApiFailure &&
                  (error.status == 401 || error.status == 403)) {
            _items = const [];
            _cursor = null;
          }
          _notice = _message(error);
        });
    } finally {
      if (_active(epoch)) setState(() => _busy = false);
    }
  }

  Future<void> _closeDetail() async {
    final view = _view;
    _view = null;
    _detail = null;
    unawaited(_notifications.clear());
    if (mounted) setState(() {});
    _activity.update(
      foreground: _activity.foreground,
      focused: _activity.focused,
      activeConversation: null,
      blocked: _activity.blocked,
    );
    if (view != null) await view.close();
  }

  Future<void> _open(ConversationSummary conversation, bool desktop) async {
    final ready = _memberships.current;
    if (_opening || _expired || ready == null) return;
    _opening = true;
    try {
      final pending = await ready.client.store.pendingSends();
      final action = await ready.client.store.pendingAction();
      final upload = await ready.client.store.pendingUpload();
      if (!mounted || _expired || !identical(_memberships.current, ready))
        return;
      if ((upload != null &&
              (upload.conversationId != conversation.id ||
                  upload.actingMembershipId != ready.membership.id)) ||
          action != null ||
          _view?.sending == true ||
          pending.any(
            (p) =>
                p.conversationId != conversation.id ||
                p.actingMembershipId != ready.membership.id,
          )) {
        setState(
          () => _notice = action != null
              ? '请先重试或明确放弃原操作，再切换会话。'
              : '请先重试或明确放弃原消息，再切换会话。',
        );
        return;
      }
      await _closeDetail();
      if (!mounted || _expired || !identical(_memberships.current, ready))
        return;
      final view = ConversationView(
        _memberships,
        _activity,
        sendAllowed: _statuses[conversation.id] != 'policy_blocked',
        onAuthorizedPage: (page) => unawaited(_notifications.acceptPage(page)),
      );
      _view = view;
      final client = ClientContext(
        session: ready.client.session,
        actingMembershipId: ready.membership.id,
        device: ready.device,
        generation: ready.client.session.generation,
        conversationId: conversation.id,
      );
      _notifications.bind(
        client,
        after: conversation.lastSeq,
        isCurrent: () =>
            mounted &&
            !_expired &&
            identical(_memberships.current, ready) &&
            identical(_view, view) &&
            view.context?.sameScope(client) == true,
        onTap: (context) => _openNotification(context, conversation.kind),
      );
      unawaited(
        view.select(client, conversation).catchError((Object error) {
          if (mounted && identical(_view, view))
            setState(() => _notice = _message(error));
        }),
      );
      if (desktop)
        setState(() => _detail = conversation);
      else {
        await Navigator.of(context).push<void>(
          MaterialPageRoute(
            builder: (context) => MessagePage(
              view: view,
              conversation: conversation,
              onBack: () => Navigator.of(context).pop(),
            ),
          ),
        );
        if (identical(_view, view)) await _closeDetail();
      }
    } catch (error) {
      await _closeDetail();
      if (mounted) setState(() => _notice = _message(error));
    } finally {
      _opening = false;
    }
  }

  Future<void> _directory(DirectoryMode mode) async {
    if (_busy || _opening || _memberships.current == null) return;
    _opening = true;
    try {
      await _closeDetail();
      if (!mounted || _memberships.current == null) return;
      await Navigator.push<void>(
        context,
        MaterialPageRoute(
          builder: (_) => DirectoryPage(memberships: _memberships, mode: mode),
        ),
      );
      if (mounted && _memberships.current != null)
        await _loadList(
          kind: mode == DirectoryMode.create ? 'group' : 'direct',
        );
    } finally {
      _opening = false;
      if (mounted) setState(() {});
    }
  }

  Future<void> _manage(ConversationSummary group) async {
    if (_busy || _opening || _memberships.current == null) return;
    _opening = true;
    try {
      await _closeDetail();
      if (!mounted || _memberships.current == null) return;
      await Navigator.push<void>(
        context,
        MaterialPageRoute(
          builder: (_) => GroupPage(memberships: _memberships, group: group),
        ),
      );
      if (mounted && _memberships.current != null) await _loadList();
    } finally {
      _opening = false;
      if (mounted) setState(() {});
    }
  }

  Future<void> _devices() async {
    if (_busy || _opening || _expired || widget.owner.current == null) return;
    _opening = true;
    try {
      await _closeDetail();
      if (!mounted || _expired || widget.owner.current == null) return;
      final stopped = await Navigator.push<bool>(
        context,
        MaterialPageRoute(
          builder: (_) => DevicePage(
            owner: widget.owner,
            memberships: _memberships,
            actingMembershipId: _ready?.membership.id ?? _attemptedMembership,
            onRetryLogoutCleanup: widget.onLogout,
          ),
        ),
      );
      if (!mounted) return;
      if (stopped == true) {
        await _logout();
        return;
      }
      setState(() {
        _ready = _memberships.current;
        if (_ready == null) {
          _items = const [];
          _cursor = null;
        }
      });
      if (_ready != null) await _loadList();
    } finally {
      _opening = false;
      if (mounted) setState(() {});
    }
  }

  Future<void> _switchMembership() async {
    if (_busy || _opening || _expired) return;
    final epoch = ++_epoch;
    setState(() {
      _busy = true;
      _notice = null;
    });
    _expiry?.cancel();
    widget.onSessionChanged?.call(null);
    try {
      // The account owner freezes old admissions before its serialized queue
      // preflight. Re-authentication creates a fresh generation/store owner.
      await _closeDetail();
      final switching = widget.owner.prepareMembershipSwitch();
      final fresh = await switching;
      // The login route remains the account owner after this route is popped.
      // Hand off only this still-current session even when the shell is gone.
      if (identical(widget.owner.current, fresh))
        widget.onSessionChanged?.call(fresh.session);
      if (!mounted || epoch != _epoch) return;
      await _closeDetail();
      if (!mounted || epoch != _epoch) return;
      _lists.close();
      _memberships.close();
      _memberships = MembershipController(widget.owner);
      _lists = ConversationList(_memberships);
      setState(() {
        _ready = null;
        _roles = const [];
        _items = const [];
        _statuses = const {};
        _cursor = null;
        _expired = false;
      });
      _armExpiry();
      final roles = await _memberships.load();
      if (_active(epoch)) setState(() => _roles = roles);
    } catch (error) {
      if (mounted && epoch == _epoch) {
        final restored = widget.owner.current;
        widget.onSessionChanged?.call(restored?.session);
        if (restored != null)
          _armExpiry();
        else {
          await _closeDetail();
          _memberships.close();
          _lists.close();
          _ready = null;
          _roles = const [];
          _items = const [];
          _expired = true;
        }
        if (mounted && epoch == _epoch)
          setState(() => _notice = _message(error));
      }
    } finally {
      if (mounted && epoch == _epoch) setState(() => _busy = false);
    }
  }

  Future<void> _logout() async {
    if (_busy) return;
    final epoch = ++_epoch;
    await _closeDetail();
    _expiry?.cancel();
    _memberships.close();
    _lists.close();
    setState(() {
      _busy = true;
      _ready = null;
      _roles = const [];
      _items = const [];
      _notice = null;
    });
    try {
      final cleaned = await widget.onLogout();
      if (!mounted || epoch != _epoch) return;
      if (cleaned) {
        if (Navigator.of(context).canPop())
          Navigator.of(context).pop();
        else
          setState(() => _expired = true);
      } else
        setState(() => _logoutFailed = true);
    } catch (_) {
      if (mounted && epoch == _epoch) setState(() => _logoutFailed = true);
    } finally {
      if (mounted && epoch == _epoch) setState(() => _busy = false);
    }
  }

  static String _message(Object error) {
    if (error is VaultFailure) return '系统安全存储不可用，设备验证未完成。请检查后重试。';
    final code = error is ApiFailure
        ? error.code
        : error is AuthFailure
        ? error.code
        : null;
    return switch (code) {
      'send_context_mismatch' => '有另一任职的消息尚未确认，请选择原任职处理。',
      'operation_context_mismatch' => '有另一任职的操作尚未确认，请选择原任职处理。',
      'send_unconfirmed' => '请先重试或明确放弃原消息，再切换任职。',
      'operation_unconfirmed' => '请先重试或明确放弃原操作，再切换任职。',
      'registration_unconfirmed' => '请先完成原设备登记。',
      'upload_unconfirmed' => '请先完成或明确放弃原文件操作，再切换任职。',
      'upload_context_mismatch' => '有原文件操作尚未确认，请回原任职和会话处理。',
      'device_revocation_unconfirmed' => '设备撤销尚未确认，请进入设备管理重试原撤销。',
      'device_quota' => '设备额度已满，请先在设备管理中撤销不用的设备。',
      'registration_exists' => '该登记已被服务端处理，凭据未保存。请在设备管理中撤销该设备后重新登记。',
      'registration_context_mismatch' => '有另一任职的设备登记尚未确认，请选择原任职重试。',
      'invalid_device' => '设备凭据已失效，请在设备管理中处理后重新验证。',
      'invalid_identity' ||
      'identity_changed' ||
      'reauthenticate' => '身份或任职已失效，请重新登录。',
      'rate_limited' => '请求过于频繁，请稍后重试。',
      'timeout' || 'unavailable' => '网络请求未完成，请重试。设备登记会沿用原请求。',
      'cancelled' => '操作已取消。',
      _ => '无法读取工作空间，请检查连接后重试。',
    };
  }

  Future<void> _enableNotifications() async {
    if (_requestingNotifications || _expired) return;
    setState(() => _requestingNotifications = true);
    final allowed = await _notifications.enable();
    if (!mounted) return;
    setState(() {
      _requestingNotifications = false;
      _notice = allowed ? '已请求消息通知，显示方式由系统设置决定。' : '消息通知未启用，不影响正常收发。';
    });
  }

  Future<void> _openNotification(ClientContext context, String kind) async {
    final ready = _ready, epoch = _epoch;
    if (_busy ||
        _opening ||
        _expired ||
        _activity.blocked ||
        ready == null ||
        _view?.context?.sameScope(context) != true)
      return;
    final lists = ConversationList(_memberships);
    try {
      final page = await lists.load(kind);
      if (!_active(epoch) ||
          !identical(_memberships.current, ready) ||
          _view?.context?.sameScope(context) != true ||
          _activity.blocked)
        return;
      final matches = page.items.where(
        (item) => item.id == context.conversationId,
      );
      if (matches.isEmpty) return;
      // _open creates a new view and authorized sync; payload is never content authority.
      if (!mounted) return;
      await _open(matches.single, MediaQuery.sizeOf(this.context).width >= 800);
    } finally {
      lists.close();
    }
  }

  @override
  void dispose() {
    unawaited(_notifications.close());
    _epoch++;
    _expiry?.cancel();
    final view = _view;
    if (view != null) unawaited(view.close());
    _activity.update(
      foreground: _activity.foreground,
      focused: _activity.focused,
      activeConversation: null,
      blocked: _activity.blocked,
    );
    _native?.dispose();
    _lists.close();
    _memberships.close();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => Scaffold(
    appBar: AppBar(
      title: const Text('集团工作空间'),
      actions: [
        if (!_expired &&
            !_logoutFailed &&
            (widget.owner.platform == 'macos' ||
                widget.owner.platform == 'windows'))
          IconButton(
            tooltip: '启用消息通知',
            onPressed: _requestingNotifications ? null : _enableNotifications,
            icon: const Icon(Icons.notifications_outlined),
          ),
        if (!_expired && !_logoutFailed)
          TextButton(
            onPressed: _busy || _opening ? null : _devices,
            child: const Text('设备管理'),
          ),
        TextButton(
          onPressed: _busy ? null : _logout,
          child: Text(_logoutFailed ? '重试退出清理' : '退出登录'),
        ),
      ],
    ),
    body: SafeArea(
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.stretch,
        children: [
          if (_busy) const LinearProgressIndicator(),
          if (_notice != null)
            Padding(
              padding: const EdgeInsets.all(16),
              child: Semantics(liveRegion: true, child: Text(_notice!)),
            ),
          if (_logoutFailed)
            const Padding(
              padding: EdgeInsets.all(16),
              child: Text('退出清理未完成，请重试。'),
            ),
          if (!_expired && !_logoutFailed && _ready == null)
            Expanded(
              child: ListView(
                padding: const EdgeInsets.all(16),
                children: [
                  Text(
                    '选择当前任职',
                    style: Theme.of(context).textTheme.headlineSmall,
                  ),
                  const SizedBox(height: 8),
                  const Text('以所选组织任职访问通讯录和会话。'),
                  for (final role in _roles)
                    Card(
                      child: ListTile(
                        key: ValueKey('membership-${role.id}'),
                        title: Text(role.organizationName),
                        subtitle: Text(
                          [
                            role.legalEntityName,
                            if (role.title.isNotEmpty) role.title,
                          ].join(' · '),
                        ),
                        trailing: role.isPrimary ? const Text('主要任职') : null,
                        onTap: _busy ? null : () => _select(role.id),
                      ),
                    ),
                  if (!_busy && _roles.isEmpty)
                    const Text('当前账号没有可用任职，请联系管理员。'),
                  if (!_busy)
                    TextButton(
                      onPressed: _loadRoles,
                      child: const Text('重新读取任职'),
                    ),
                ],
              ),
            ),
          if (!_expired && !_logoutFailed && _ready != null) ...[
            Padding(
              padding: const EdgeInsets.all(16),
              child: Text(
                '${_ready!.membership.organizationName} · ${_ready!.client.session.displayName}',
              ),
            ),
            Padding(
              padding: const EdgeInsets.symmetric(horizontal: 16),
              child: Wrap(
                spacing: 8,
                runSpacing: 8,
                children: [
                  SegmentedButton<String>(
                    segments: const [
                      ButtonSegment(value: 'direct', label: Text('单聊')),
                      ButtonSegment(value: 'group', label: Text('群聊')),
                    ],
                    selected: {_kind},
                    onSelectionChanged: _busy
                        ? null
                        : (value) => _loadList(kind: value.single),
                  ),
                  TextButton(
                    onPressed: _busy || _opening
                        ? null
                        : () => _directory(DirectoryMode.browse),
                    child: const Text('通讯录'),
                  ),
                  TextButton(
                    onPressed: _busy || _opening
                        ? null
                        : () => _directory(DirectoryMode.create),
                    child: const Text('创建群聊'),
                  ),
                  TextButton(
                    onPressed: _busy || _opening ? null : _switchMembership,
                    child: const Text('切换任职'),
                  ),
                  IconButton(
                    tooltip: '刷新会话列表',
                    onPressed: _busy ? null : () => _loadList(),
                    icon: const Icon(Icons.refresh),
                  ),
                ],
              ),
            ),
            Expanded(
              child: LayoutBuilder(
                builder: (context, size) => Row(
                  crossAxisAlignment: CrossAxisAlignment.stretch,
                  children: [
                    SizedBox(
                      width: size.maxWidth >= 800 ? 360 : size.maxWidth,
                      child: ListView(
                        padding: const EdgeInsets.all(16),
                        children: [
                          for (final item in _items)
                            Card(
                              child: ListTile(
                                onTap: _busy || _opening
                                    ? null
                                    : () => _open(item, size.maxWidth >= 800),
                                trailing: item.kind == 'group'
                                    ? IconButton(
                                        tooltip: '管理${item.name}',
                                        onPressed: _busy || _opening
                                            ? null
                                            : () => _manage(item),
                                        icon: const Icon(
                                          Icons.settings_outlined,
                                        ),
                                      )
                                    : null,
                                leading: Icon(
                                  item.kind == 'group'
                                      ? Icons.groups
                                      : Icons.chat_bubble_outline,
                                ),
                                title: Text(item.name),
                                subtitle: Text(
                                  _statuses[item.id] == 'policy_blocked'
                                      ? '通信策略已阻断'
                                      : '${item.kind == 'group' ? '群聊' : '单聊'}',
                                ),
                              ),
                            ),
                          if (_items.isEmpty && !_busy) const Text('暂无会话'),
                          if (_cursor != null)
                            TextButton(
                              onPressed: _busy
                                  ? null
                                  : () => _loadList(more: true),
                              child: const Text('加载更多'),
                            ),
                        ],
                      ),
                    ),
                    if (size.maxWidth >= 800) ...[
                      const VerticalDivider(width: 1),
                      Expanded(
                        child: _detail == null || _view == null
                            ? const Center(child: Text('选择会话开始沟通'))
                            : MessagePage(
                                key: ValueKey(_view),
                                view: _view!,
                                conversation: _detail!,
                                onBack: () => unawaited(_closeDetail()),
                              ),
                      ),
                    ],
                  ],
                ),
              ),
            ),
          ],
          if (_expired && !_busy)
            TextButton(
              onPressed: () => Navigator.of(context).maybePop(),
              child: const Text('返回登录页'),
            ),
        ],
      ),
    ),
  );
}
