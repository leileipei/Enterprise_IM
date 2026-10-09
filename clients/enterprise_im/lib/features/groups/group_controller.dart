import '../../core/protocol/api_client.dart';
import '../../core/protocol/client_id.dart';
import '../../core/protocol/models.dart';
import '../../core/protocol/seq.dart';
import '../../core/storage/records.dart';
import '../memberships/membership_controller.dart';
import '../memberships/workspace_requests.dart';
export '../../core/storage/records.dart' show ActionKind;

String groupRole(Map<String, Object?> data) {
  final role = stringField(data, 'role', max: 16);
  if (!const ['owner', 'admin', 'member'].contains(role))
    throw const FormatException('群角色无效');
  return role;
}

String groupStatus(Map<String, Object?> data, String key) {
  final value = stringField(data, key, max: 32);
  if (!const ['active', 'policy_blocked'].contains(value))
    throw const FormatException('群状态无效');
  return value;
}

final class GroupAccess {
  final String intervalId, role, status;
  final Seq joinSeq;
  GroupAccess._(this.intervalId, this.role, this.status, this.joinSeq);
  factory GroupAccess.fromJson(Map<String, Object?> data) => GroupAccess._(
    uuidField(data, 'interval_id'),
    groupRole(data),
    groupStatus(data, 'group_status'),
    Seq.fromWire(data['join_seq'], legacyInteger: true),
  );
}

final class GroupMember {
  final String intervalId, name, role, organization;
  GroupMember._(this.intervalId, this.name, this.role, this.organization);
  factory GroupMember.fromJson(Map<String, Object?> d) => GroupMember._(
    uuidField(d, 'interval_id'),
    stringField(d, 'display_name', max: 1024),
    groupRole(d),
    stringField(d, 'organization_name', max: 1024),
  );
}

final class RosterPage {
  final List<GroupMember> members;
  final String? cursor;
  RosterPage._(this.members, this.cursor);
  factory RosterPage.fromJson(Map<String, Object?> data, String? previous) {
    final raw = data['members'];
    if (raw is! List || raw.length > 50) throw const FormatException('群成员列表无效');
    final seen = <String>{}, members = <GroupMember>[];
    for (final row in raw) {
      final member = GroupMember.fromJson(objectMap(row));
      if (!seen.add(member.intervalId)) throw const FormatException('重复成员');
      members.add(member);
    }
    final more = boolField(data, 'has_more');
    String? cursor;
    if (more) {
      cursor = stringField(data, 'next_cursor', max: 256);
      if (cursor == previous || RegExp(r'[\x00-\x20\x7f]').hasMatch(cursor))
        throw const FormatException('群游标无效');
    } else if (data.containsKey('next_cursor'))
      throw const FormatException('群游标无效');
    return RosterPage._(List.unmodifiable(members), cursor);
  }
}

final class ActionResult {
  final String? conversationId, status;
  final Seq sequence;
  ActionResult._(this.conversationId, this.status, this.sequence);
  factory ActionResult.parse(PendingAction action, Map<String, Object?> data) {
    final kind = action.kind;
    if (kind == ActionKind.create || kind == ActionKind.direct) {
      if (data['type'] != (kind == ActionKind.create ? 'group' : 'direct'))
        throw const FormatException('会话响应类型无效');
      Seq.fromWire(data['policy_version'], legacyInteger: true);
      if (kind == ActionKind.create) {
        final count = data['member_count'];
        if (count is! int || count < 2 || count > 21)
          throw const FormatException('群成员数量无效');
      } else {
        boolField(data, 'cross_legal');
        stringField(data, 'decision_reason', max: 128);
      }
      return ActionResult._(
        uuidField(data, 'id'),
        null,
        Seq.fromWire(data['last_seq'], legacyInteger: true),
      );
    }
    if (kind == ActionKind.invite) {
      uuidField(data, 'interval_id');
      Seq.fromWire(data['policy_version'], legacyInteger: true);
      return ActionResult._(
        action.target,
        null,
        Seq.fromWire(data['join_seq'], legacyInteger: true),
      );
    }
    if (kind == ActionKind.transfer) {
      if (uuidField(data, 'source_interval_id') !=
              action.body!['source_interval_id'] ||
          uuidField(data, 'target_interval_id') !=
              action.body!['target_interval_id'])
        throw const FormatException('群主转让区间不匹配');
      return ActionResult._(action.target, null, Seq.parse('0'));
    }
    if (kind == ActionKind.recheck) {
      Seq.fromWire(data['policy_version'], legacyInteger: true);
      return ActionResult._(
        action.target,
        groupStatus(data, 'status'),
        Seq.parse('0'),
      );
    }
    final status = kind == ActionKind.leave ? 'left' : 'removed';
    if (uuidField(data, 'interval_id') != action.body!['interval_id'] ||
        data['status'] != status)
      throw const FormatException('退群或移除结果不匹配');
    return ActionResult._(
      action.target,
      status,
      Seq.fromWire(data['leave_seq'], legacyInteger: true),
    );
  }
}

final class GroupController {
  final WorkspaceRequests requests;
  bool _busy = false;
  GroupController(MembershipController memberships)
    : requests = WorkspaceRequests(memberships);
  Future<GroupAccess> membership(String group) async {
    uuidField({'id': group}, 'id');
    return GroupAccess.fromJson(
      await requests.request('GET', '/api/v1/groups/$group/membership'),
    );
  }

  Future<RosterPage> roster(String group, {String? cursor}) async {
    uuidField({'id': group}, 'id');
    return RosterPage.fromJson(
      await requests.request(
        'GET',
        Uri(
          path: '/api/v1/groups/$group/members',
          queryParameters: {
            'limit': '50',
            if (cursor != null) 'cursor': cursor,
          },
        ).toString(),
      ),
      cursor,
    );
  }

  Future<PendingAction?> pending() async {
    final ready = requests.ready, epoch = requests.epoch;
    final action = await ready.client.store.pendingAction();
    requests.check(ready, epoch);
    return action;
  }

  Future<ActionResult> startDirect(String role) =>
      _new(ActionKind.direct, '', {'target_membership_id': role});
  Future<ActionResult> create(String name, List<String> members) => _new(
    ActionKind.create,
    '',
    {'name': name.trim(), 'member_membership_ids': members},
  );
  Future<ActionResult> invite(String group, String role) =>
      _new(ActionKind.invite, group, {'target_membership_id': role});
  Future<ActionResult> remove(String group, String interval) =>
      _new(ActionKind.remove, group, {'interval_id': interval});
  Future<ActionResult> leave(String group, String interval) =>
      _new(ActionKind.leave, group, {'interval_id': interval});
  Future<ActionResult> transfer(String group, String source, String target) =>
      _new(ActionKind.transfer, group, {
        'source_interval_id': source,
        'target_interval_id': target,
      });
  Future<ActionResult> recheck(String group) =>
      _new(ActionKind.recheck, group, null);
  Future<ActionResult> _new(
    ActionKind kind,
    String target,
    Map<String, Object?>? body,
  ) async {
    if (_busy) throw const ApiFailure('context_in_progress');
    _busy = true;
    try {
      final ready = requests.ready, epoch = requests.epoch;
      if (await pending() != null ||
          (await ready.client.store.pendingSends()).isNotEmpty)
        throw const ApiFailure('operation_unconfirmed');
      requests.check(ready, epoch);
      final id = ClientMessageId.create(ready.clock);
      final payload = body == null
          ? null
          : {
              ...body,
              if (const [
                ActionKind.create,
                ActionKind.invite,
                ActionKind.transfer,
              ].contains(kind))
                'client_request_id': id,
            };
      final action = PendingAction(
        id: id,
        actingMembershipId: ready.membership.id,
        target: target,
        kind: kind,
        generation: ready.client.session.generation,
        body: payload,
      );
      await ready.client.store.putPendingAction(action);
      requests.check(ready, epoch);
      return await _send(action, ready, epoch);
    } finally {
      _busy = false;
    }
  }

  Future<ActionResult> _send(
    PendingAction action,
    WorkspaceAdmission ready,
    int epoch,
  ) async {
    if (action.actingMembershipId != ready.membership.id)
      throw const ApiFailure('operation_context_mismatch');
    final data = await requests.request('POST', action.path, body: action.body);
    requests.check(ready, epoch);
    final result = ActionResult.parse(action, data);
    if (action.kind == ActionKind.leave) {
      await ready.client.store.clearReceivedState();
      requests.check(ready, epoch);
    }
    await ready.client.store.finishAction(action);
    requests.check(ready, epoch);
    return result;
  }

  Future<ActionResult> retry() async {
    if (_busy) throw const ApiFailure('context_in_progress');
    _busy = true;
    try {
      final ready = requests.ready, epoch = requests.epoch;
      final action = await pending();
      if (action == null) throw const ApiFailure('no_pending_operation');
      return await _send(action, ready, epoch);
    } finally {
      _busy = false;
    }
  }

  Future<void> abandon() async {
    if (_busy) throw const ApiFailure('context_in_progress');
    _busy = true;
    try {
      final ready = requests.ready,
          epoch = requests.epoch,
          action = await pending();
      if (action == null) return;
      if (action.actingMembershipId != ready.membership.id)
        throw const ApiFailure('operation_context_mismatch');
      await ready.client.store.finishAction(action);
      requests.check(ready, epoch);
    } finally {
      _busy = false;
    }
  }

  void close() => requests.close();
}
