import 'dart:async';
import 'package:flutter/material.dart';
import '../core/protocol/models.dart';
import '../features/directory/directory_controller.dart';
import '../features/groups/group_controller.dart';
import '../features/memberships/membership_controller.dart';
import 'directory_page.dart';
import 'workspace_action_page.dart';

class GroupPage extends StatefulWidget {
  final MembershipController memberships;
  final ConversationSummary group;
  const GroupPage({super.key, required this.memberships, required this.group});
  @override
  State<GroupPage> createState() => _GroupPageState();
}

class _GroupPageState extends WorkspaceActionState<GroupPage> {
  @override
  MembershipController get memberships => widget.memberships;
  GroupAccess? _access;
  List<GroupMember> _members = const [];
  String? _cursor;
  bool _left = false;
  @override
  bool get pendingMatches => pending?.target == widget.group.id;
  bool get _manager => _access?.role == 'owner' || _access?.role == 'admin';
  @override
  void initState() {
    super.initState();
    unawaited(run(_load));
  }

  @override
  void withdraw() {
    _access = null;
    _members = const [];
    _cursor = null;
  }

  Future<void> _load() async {
    withdraw();
    final access = await actions.membership(widget.group.id);
    if (!active) return;
    _access = access;
    if (_manager) await _roster();
  }

  Future<void> _roster({bool more = false}) async {
    final page = await actions.roster(
      widget.group.id,
      cursor: more ? _cursor : null,
    );
    if (!active) return;
    _members = List.unmodifiable(
      {
        if (more)
          for (final member in _members) member.intervalId: member,
        for (final member in page.members) member.intervalId: member,
      }.values,
    );
    _cursor = page.cursor;
  }

  @override
  Future<void> afterAction(ActionResult result) async {
    if (result.status == 'left') {
      _left = true;
      withdraw();
    } else if (!_left) {
      try {
        await _load();
      } catch (_) {
        withdraw();
        notice = '操作已确认，但群状态刷新失败，请刷新群状态核实结果。';
      }
    }
  }

  Future<void> _invite() async {
    final role = await Navigator.push<DirectoryRole>(
      context,
      MaterialPageRoute(
        builder: (_) =>
            DirectoryPage(memberships: memberships, mode: DirectoryMode.pick),
      ),
    );
    if (role != null && active)
      await execute(() => actions.invite(widget.group.id, role.id));
  }

  @override
  Widget build(BuildContext context) {
    final enabled =
        active && !busy && pending == null && !_left && _access != null;
    return Scaffold(
      appBar: pageBar('群管理'),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(16),
          children: [
            if (!expired)
              Text(
                widget.group.name,
                style: Theme.of(context).textTheme.titleLarge,
              ),
            operationStatus,
            if (!expired && !_left) ...[
              if (_access != null)
                Text(
                  '当前角色：${_access!.role} · ${_access!.status == 'policy_blocked' ? '通信策略已阻断' : '通信正常'}',
                ),
              TextButton(
                onPressed: busy ? null : () => run(_load),
                child: const Text('刷新群状态'),
              ),
              if (_manager)
                Wrap(
                  spacing: 8,
                  children: [
                    FilledButton(
                      onPressed: enabled ? _invite : null,
                      child: const Text('邀请人员'),
                    ),
                    TextButton(
                      onPressed: enabled
                          ? () =>
                                execute(() => actions.recheck(widget.group.id))
                          : null,
                      child: const Text('重新检查通信策略'),
                    ),
                  ],
                ),
              for (final member in _members)
                Card(
                  child: ListTile(
                    title: Text(member.name),
                    subtitle: Text('${member.organization} · ${member.role}'),
                    trailing: Wrap(
                      children: [
                        if (_access?.role == 'owner' &&
                            member.intervalId != _access?.intervalId)
                          TextButton(
                            onPressed: enabled
                                ? () async {
                                    if (await confirm(
                                          '转让群主？',
                                          '将群主转让给 ${member.name}（${member.organization}）。',
                                          '确认转让',
                                        ) &&
                                        active) {
                                      await execute(
                                        () => actions.transfer(
                                          widget.group.id,
                                          _access!.intervalId,
                                          member.intervalId,
                                        ),
                                      );
                                    }
                                  }
                                : null,
                            child: const Text('转让群主'),
                          ),
                        if (member.role != 'owner' &&
                            member.intervalId != _access?.intervalId &&
                            (_access?.role == 'owner' ||
                                member.role == 'member'))
                          TextButton(
                            onPressed: enabled
                                ? () async {
                                    if (await confirm(
                                          '移除成员？',
                                          '移除 ${member.name}（${member.organization}）的当前群成员身份。',
                                          '确认移除',
                                        ) &&
                                        active) {
                                      await execute(
                                        () => actions.remove(
                                          widget.group.id,
                                          member.intervalId,
                                        ),
                                      );
                                    }
                                  }
                                : null,
                            child: const Text('移除'),
                          ),
                      ],
                    ),
                  ),
                ),
              if (_cursor != null)
                TextButton(
                  onPressed: busy ? null : () => run(() => _roster(more: true)),
                  child: const Text('加载更多成员'),
                ),
              if (_access?.role == 'owner') const Text('退出前请先转让群主。'),
              if (_access != null && _access!.role != 'owner')
                TextButton(
                  onPressed: enabled
                      ? () async {
                          if (await confirm(
                                '退出群聊？',
                                '退出后将停止接收该群的新消息。',
                                '确认退出',
                              ) &&
                              active) {
                            await execute(
                              () => actions.leave(
                                widget.group.id,
                                _access!.intervalId,
                              ),
                            );
                          }
                        }
                      : null,
                  child: const Text('退出群聊'),
                ),
            ],
          ],
        ),
      ),
    );
  }
}
