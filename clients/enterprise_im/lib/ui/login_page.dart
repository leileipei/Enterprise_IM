import 'dart:async';
import 'dart:io';
import '../platform/native_storage.dart';
import '../core/auth/client_session.dart';
import '../core/auth/account_reference.dart';
import '../core/auth/account_selector.dart';
import '../core/auth/session_store.dart';
import '../core/storage/secure_vault.dart';
import 'package:flutter/material.dart';
import '../core/auth/native_auth.dart';
import '../core/protocol/api_client.dart';
import '../platform/browser_launcher.dart';
import '../platform/login_callback.dart';
import 'client_shell.dart';
import '../platform/app_activity.dart';

class LoginPage extends StatefulWidget {
  final String platform;
  final AppActivity? activity;
  final LoginCallback Function(String, String) callbackFactory;
  final bool platformReady;
  final ApiClient Function(Uri)? apiFactory;
  final BrowserLauncher? browser;
  final Future<SessionStore> Function()? storeFactory;
  const LoginPage({
    super.key,
    required this.platform,
    required this.callbackFactory,
    this.platformReady = true,
    this.activity,
    this.apiFactory,
    this.browser,
    this.storeFactory,
  });
  @override
  State<LoginPage> createState() => _LoginPageState();
}

class _LoginPageState extends State<LoginPage> {
  final _address = TextEditingController();
  NativeAuth? _auth;
  ApiClient? _api;
  AuthSession? _session;
  ClientSession? _client;
  AccountReference? _account;
  bool _hasSavedRefresh = false;
  Timer? _expiry;
  bool _busy = false, _starting = false;
  bool _startupReadFailed = false, _cleanupFailed = false;
  Future<SessionStore>? _storeFuture;
  SessionStore? _sessions;
  String? _notice;
  int _generation = 0;
  @override
  void initState() {
    super.initState();
    unawaited(_restoreSaved());
  }

  Future<SessionStore> _store() {
    final existing = _storeFuture;
    if (existing != null) return existing;
    late Future<SessionStore> pending;
    pending = (widget.storeFactory?.call() ?? _defaultStore())
        .then((sessions) async {
          if (!mounted) {
            await sessions.close();
            throw const AuthCancelled();
          }
          _sessions = sessions;
          return sessions;
        })
        .catchError((Object error, StackTrace stack) {
          if (identical(_storeFuture, pending)) _storeFuture = null;
          Error.throwWithStackTrace(error, stack);
        });
    return _storeFuture = pending;
  }

  ClientSession _clientFor(Uri origin, SessionStore sessions) {
    _api?.close();
    final api =
        widget.apiFactory?.call(origin) ??
        ApiClient(origin, HttpApiTransport());
    final auth = NativeAuth(
      api,
      callbackFactory: widget.callbackFactory,
      browser: widget.browser ?? ExternalBrowserLauncher(),
    );
    _api = api;
    _auth = auth;
    return _client = ClientSession(auth, sessions, platform: widget.platform);
  }

  Future<void> _restoreSaved() async {
    if (_busy) return;
    final generation = ++_generation;
    var reading = true;
    setState(() {
      _busy = true;
      _starting = true;
      _startupReadFailed = false;
      _notice = null;
    });
    try {
      final sessions = await _store();
      if (!mounted || generation != _generation) return;
      final account = await AccountSelector(sessions.vault).load();
      if (!mounted || generation != _generation || account == null) return;
      reading = false;
      setState(() {
        _address.text = account.origin.origin;
        _account = account;
        _hasSavedRefresh = true;
      });
      final admitted = await _clientFor(
        account.origin,
        sessions,
      ).restore(account);
      _showSession(admitted.session, generation);
    } on VaultFailure {
      if (mounted && generation == _generation)
        setState(() {
          _session = null;
          _startupReadFailed = reading;
          _notice = reading
              ? '无法读取上次账号，请检查系统安全存储后重试。'
              : '系统安全存储不可用，登录未完成。请检查系统配置后重试。';
        });
    } on AuthFailure catch (error) {
      if (mounted && generation == _generation)
        setState(() {
          _session = null;
          if (error.code == 'reauthenticate') _hasSavedRefresh = false;
          _notice = _message(error.code);
        });
    } catch (_) {
      if (mounted && generation == _generation)
        setState(() {
          _session = null;
          _startupReadFailed = reading;
          _notice = reading ? '无法读取上次账号，请检查系统安全存储后重试。' : '登录暂时不可用，请稍后重试。';
        });
    } finally {
      if (mounted && generation == _generation)
        setState(() {
          _busy = false;
          _starting = false;
        });
    }
  }

  bool get _restoreMatchesAddress {
    try {
      return _account?.origin ==
          ApiClient.validateOrigin(Uri.parse(_address.text.trim()));
    } catch (_) {
      return false;
    }
  }

  Future<void> _login() async {
    if (_busy || _cleanupFailed || !widget.platformReady) return;
    final generation = ++_generation;
    setState(() {
      _busy = true;
      _notice = null;
    });
    try {
      final origin = ApiClient.validateOrigin(Uri.parse(_address.text.trim()));
      _expiry?.cancel();
      setState(() {
        _session = null;
        _startupReadFailed = false;
      });
      await _client?.logout();
      await _auth?.cancel();
      _account = null;
      _hasSavedRefresh = false;
      _api?.close();
      if (!mounted || generation != _generation) return;
      final sessions = await _store();
      if (!mounted || generation != _generation) return;
      final client = _clientFor(origin, sessions);
      final admitted = await client.signIn();
      _showSession(admitted.session, generation);
    } on VaultFailure {
      if (mounted && generation == _generation)
        setState(() {
          _session = null;
          _notice = '系统安全存储不可用，登录未完成。请检查系统配置后重试。';
        });
    } on FormatException {
      if (mounted && generation == _generation)
        setState(
          () => _notice = '请输入有效的 HTTPS 服务根地址，例如 https://im.example.com。',
        );
    } on AuthFailure catch (error) {
      if (mounted && generation == _generation)
        setState(() {
          _session = _client?.current?.session;
          _notice = _message(error.code);
        });
    } catch (_) {
      if (mounted && generation == _generation)
        setState(() => _notice = '登录暂时不可用，请稍后重试。');
    } finally {
      if (mounted && generation == _generation) setState(() => _busy = false);
    }
  }

  void _showSession(AuthSession session, int generation) {
    if (!mounted || generation != _generation) return;
    _expiry?.cancel();
    setState(() {
      _session = session;
      _account = AccountReference.fromSession(session);
      _hasSavedRefresh = _auth?.canRefresh == true;
      _notice = null;
    });
    _expiry = Timer(session.remaining, () {
      if (mounted &&
          generation == _generation &&
          identical(_session, session)) {
        unawaited(
          (_client?.close() ?? Future<void>.value()).catchError(_reportClose),
        );
        setState(() {
          _session = null;
          _notice = '登录已到期，请恢复登录或重新登录。';
        });
      }
    });
  }

  void _workspaceSessionChanged(AuthSession? session) {
    if (!mounted ||
        session != null && !identical(_client?.current?.session, session))
      return;
    final generation = ++_generation;
    _expiry?.cancel();
    if (session == null)
      setState(() => _session = null);
    else
      _showSession(session, generation);
  }

  Future<void> _refresh() async {
    final auth = _auth, client = _client, account = _account;
    if (_busy ||
        !_restoreMatchesAddress ||
        auth == null ||
        client == null ||
        account == null)
      return;
    final generation = ++_generation;
    setState(() {
      _busy = true;
      _session = null;
      _notice = null;
    });
    try {
      final admitted = await (auth.canRefresh
          ? client.refresh()
          : client.restore(account));
      _showSession(admitted.session, generation);
    } on VaultFailure {
      if (mounted && generation == _generation)
        setState(() {
          _session = null;
          _notice = '系统安全存储不可用，登录未完成。请检查系统配置后重试。';
        });
    } on AuthFailure catch (error) {
      if (mounted && generation == _generation)
        setState(() {
          _session = _client?.current?.session;
          if (error.code == 'reauthenticate') _hasSavedRefresh = false;
          _notice = _message(error.code);
        });
    } catch (_) {
      if (mounted && generation == _generation)
        setState(() {
          _session = null;
          _notice = '登录暂时不可用，请稍后重试。';
        });
    } finally {
      if (mounted && generation == _generation) setState(() => _busy = false);
    }
  }

  Future<bool> _cancel() async {
    final generation = ++_generation;
    final client = _client, auth = _auth;
    _expiry?.cancel();
    setState(() {
      _busy = true;
      _starting = false;
      _cleanupFailed = false;
      _session = null;
      _notice = '正在退出登录…';
      _hasSavedRefresh = false;
    });
    try {
      await client?.logout();
      await auth?.cancel();
      if (!mounted || generation != _generation) return false;
      setState(() {
        _account = null;
        _notice = '已取消登录。';
      });
      return true;
    } catch (_) {
      if (mounted && generation == _generation)
        setState(() {
          _cleanupFailed = true;
          _notice = '退出清理未完成，请重试。';
        });
      return false;
    } finally {
      if (mounted && generation == _generation) setState(() => _busy = false);
    }
  }

  static Future<SessionStore> _defaultStore() async {
    final support = await nativeStorageRoot();
    final directory = Directory('${support.path}/accounts');
    await directory.create(recursive: true);
    return SessionStore(OsSecureVault(), directory: directory);
  }

  static void _reportClose(Object error) => FlutterError.reportError(
    FlutterErrorDetails(
      exception: error,
      library: 'enterprise_im',
      context: ErrorDescription('closing authenticated client'),
    ),
  );

  static String _message(String code) => switch (code) {
    'cancelled' => '已取消登录。',
    'timeout' => '登录等待超时，请重试。',
    'provider_rejected' => '登录未获授权，请重试或联系管理员。',
    'reauthenticate' || 'expired' => '登录已失效，请重新登录。',
    'browser_unavailable' => '无法打开系统浏览器，请检查系统设置。',
    'native_rate_limit' => '登录请求过于频繁，请稍后重试。',
    'identity_changed' => '登录人员发生变化，请重新登录。',
    _ => '登录暂时不可用，请核对服务地址或联系管理员。',
  };
  @override
  void dispose() {
    _generation++;
    _expiry?.cancel();
    unawaited(
      (_client?.close() ??
              _sessions?.close() ??
              _auth?.cancel() ??
              Future<void>.value())
          .catchError(_reportClose),
    );
    _api?.close();
    _address.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => Scaffold(
    appBar: AppBar(title: const Text('企业 IM')),
    body: SafeArea(
      child: Center(
        child: SingleChildScrollView(
          padding: const EdgeInsets.all(24),
          child: ConstrainedBox(
            constraints: const BoxConstraints(maxWidth: 520),
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.stretch,
              children: [
                Text(
                  _session == null ? '登录集团工作空间' : '登录成功',
                  style: Theme.of(context).textTheme.headlineSmall,
                ),
                const SizedBox(height: 16),
                if (_session != null) ...[
                  Text(
                    _session!.displayName,
                    style: Theme.of(context).textTheme.titleLarge,
                  ),
                  const SizedBox(height: 8),
                  FilledButton.tonal(
                    onPressed: _busy || _client?.current == null
                        ? null
                        : () => Navigator.of(context).push<void>(
                            MaterialPageRoute(
                              builder: (_) => ClientShell(
                                owner: _client!,
                                activity: widget.activity,
                                onLogout: _cancel,
                                onSessionChanged: _workspaceSessionChanged,
                              ),
                            ),
                          ),
                    child: const Text('进入工作空间'),
                  ),
                  const SizedBox(height: 20),
                ],
                TextField(
                  controller: _address,
                  onChanged: (_) => setState(() {}),
                  enabled: !_busy,
                  keyboardType: TextInputType.url,
                  autocorrect: false,
                  decoration: const InputDecoration(
                    labelText: '服务地址',
                    hintText: 'https://im.example.com',
                    border: OutlineInputBorder(),
                  ),
                ),
                const SizedBox(height: 12),
                const Text('使用系统浏览器完成企业账号登录。'),
                if (!widget.platformReady) ...[
                  const SizedBox(height: 12),
                  const Text('无法接收登录回调，请检查应用安装配置。'),
                ],
                if (_notice != null) ...[
                  const SizedBox(height: 12),
                  Semantics(liveRegion: true, child: Text(_notice!)),
                ],
                const SizedBox(height: 20),
                FilledButton(
                  onPressed: _busy || _cleanupFailed || !widget.platformReady
                      ? null
                      : _login,
                  child: Text(
                    _starting
                        ? '恢复上次登录…'
                        : _busy
                        ? '等待登录完成…'
                        : '使用企业账号登录',
                  ),
                ),
                if (_busy)
                  TextButton(onPressed: _cancel, child: const Text('取消登录')),
                if (_startupReadFailed)
                  TextButton(
                    onPressed: _busy ? null : _restoreSaved,
                    child: const Text('重试读取上次账号'),
                  ),
                if (_cleanupFailed)
                  TextButton(
                    onPressed: _busy ? null : _cancel,
                    child: const Text('重试退出清理'),
                  ),
                if (_hasSavedRefresh && _account != null)
                  TextButton(
                    onPressed: _busy || !_restoreMatchesAddress
                        ? null
                        : _refresh,
                    child: const Text('恢复登录'),
                  ),
                if (_session != null)
                  TextButton(
                    onPressed: () async {
                      final cleaned = await _cancel();
                      if (cleaned && mounted)
                        setState(() => _notice = '已退出登录。');
                    },
                    child: const Text('退出登录'),
                  ),
              ],
            ),
          ),
        ),
      ),
    ),
  );
}
