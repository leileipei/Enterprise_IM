import 'dart:async';
import 'package:flutter/material.dart';
import '../core/protocol/api_client.dart';
import '../core/auth/auth_session.dart';
import '../core/storage/records.dart';
import '../features/groups/group_controller.dart';
import '../features/memberships/membership_controller.dart';

// One page owns its requests and withdraws data at the earlier identity/device
// deadline. Pending mutations remain encrypted until exact ACK or explicit discard.
abstract class WorkspaceActionState<T extends StatefulWidget> extends State<T> {
  MembershipController get memberships;
  late final WorkspaceAdmission admission;
  late final GroupController actions;
  PendingAction? pending;
  bool busy = false, expired = false;
  String? notice;
  Timer? _expiry;
  int _epoch = 0;
  bool get active =>
      mounted && !expired && identical(memberships.current, admission);
  bool get pendingMatches => pending != null;
  void withdraw();
  Future<void> afterAction(ActionResult result) async {}
  @override
  void initState() {
    super.initState();
    admission = memberships.current!;
    actions = GroupController(memberships);
    _expiry = Timer(admission.remaining, () {
      _epoch++;
      expired = true;
      actions.close();
      withdraw();
      if (mounted)
        setState(() {
          busy = false;
          pending = null;
          notice = '身份或设备验证已到期，请返回工作空间重新登录。';
        });
    });
  }

  Future<void> run(Future<void> Function() operation) async {
    if (busy || !active) return;
    final epoch = ++_epoch;
    setState(() {
      busy = true;
      notice = null;
    });
    try {
      await operation();
    } catch (error) {
      if (active && epoch == _epoch) {
        if (error is ApiFailure && (error.status == 401 || error.status == 403))
          withdraw();
        notice = error is FormatException
            ? error.message.toString()
            : error is AuthFailure
            ? '请重新登录后重试。'
            : error is ApiFailure && error.code == 'operation_unconfirmed'
            ? '请先处理原消息或原操作。'
            : '操作未确认，请检查连接及权限后重试原操作。';
      }
    } finally {
      if (active && epoch == _epoch) {
        try {
          pending = await actions.pending();
        } catch (_) {
          notice = '无法读取待确认操作，请返回工作空间重试。';
        }
        if (active && epoch == _epoch) setState(() => busy = false);
      }
    }
  }

  Future<void> execute(Future<ActionResult> Function() operation) =>
      run(() async {
        final result = await operation();
        if (!active) return;
        notice = result.status == 'left' ? '已退出群聊' : '操作已确认';
        await afterAction(result);
      });
  Future<bool> confirm(String title, String explanation, String accept) async =>
      await showDialog<bool>(
        context: context,
        builder: (context) => AlertDialog(
          title: Text(title),
          content: Text(explanation),
          actions: [
            TextButton(
              onPressed: () => Navigator.pop(context, false),
              child: const Text('取消'),
            ),
            FilledButton(
              onPressed: () => Navigator.pop(context, true),
              child: Text(accept),
            ),
          ],
        ),
      ) ==
      true;
  Widget get operationStatus => Column(
    crossAxisAlignment: CrossAxisAlignment.stretch,
    children: [
      if (busy) const LinearProgressIndicator(),
      if (notice != null)
        Padding(
          padding: const EdgeInsets.all(12),
          child: Semantics(liveRegion: true, child: Text(notice!)),
        ),
      if (pending != null && !pendingMatches)
        const Text('有其他会话的原操作，请返回原群管理页处理。'),
      if (pending != null && pendingMatches) ...[
        const Text('有待确认的原操作。重试会沿用原目标和请求；放弃本地记录不会撤销服务端已经执行的操作。'),
        Wrap(
          spacing: 8,
          children: [
            FilledButton(
              onPressed: busy || !active ? null : () => execute(actions.retry),
              child: const Text('重试原操作'),
            ),
            TextButton(
              onPressed: busy || !active
                  ? null
                  : () async {
                      if (await confirm(
                            '放弃本地操作？',
                            '请先核实服务端结果。放弃只删除本地重试记录。',
                            '确认放弃',
                          ) &&
                          active) {
                        await run(() async {
                          await actions.abandon();
                          if (active) notice = '已放弃本地操作，请刷新列表核实结果。';
                        });
                      }
                    },
              child: const Text('放弃本地操作'),
            ),
          ],
        ),
      ],
    ],
  );
  AppBar pageBar(String title) => AppBar(
    title: Text(title),
    leading: IconButton(
      tooltip: '返回工作空间',
      icon: const Icon(Icons.arrow_back),
      onPressed: () => Navigator.pop(context),
    ),
  );
  @override
  void dispose() {
    _epoch++;
    _expiry?.cancel();
    actions.close();
    super.dispose();
  }
}
