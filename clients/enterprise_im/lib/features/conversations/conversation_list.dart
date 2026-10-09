import '../../core/protocol/api_client.dart';
import '../../core/protocol/models.dart';
import '../memberships/membership_controller.dart';

final class ConversationPage {
  final List<ConversationSummary> items;
  final Map<String, String> groupStatuses;
  final bool hasMore;
  final String? nextCursor;
  ConversationPage._(
    this.items,
    this.groupStatuses,
    this.hasMore,
    this.nextCursor,
  );
}

final class ConversationList {
  final MembershipController memberships;
  Cancellation? _cancel;
  int _epoch = 0;
  bool _closed = false;
  ConversationList(this.memberships);
  void _check(int epoch, WorkspaceAdmission admission) {
    if (_closed ||
        epoch != _epoch ||
        !identical(memberships.current, admission))
      throw const ApiFailure('cancelled');
  }

  Future<ConversationPage> load(String kind, {String? cursor}) async {
    if (!const ['direct', 'group'].contains(kind))
      throw const FormatException('会话类别无效');
    final admission = memberships.current;
    if (_closed || admission == null) throw const ApiFailure('reauthenticate');
    final epoch = ++_epoch;
    _cancel?.cancel();
    final cancel = Cancellation();
    _cancel = cancel;
    try {
      final path = Uri(
        path: kind == 'group' ? '/api/v1/groups' : '/api/v1/conversations',
        queryParameters: {'limit': '50', if (cursor != null) 'cursor': cursor},
      ).toString();
      final data = await memberships.owner.auth.api.json(
        'GET',
        path,
        bearer: admission.client.session.accessToken,
        actingMembershipId: admission.membership.id,
        cancellation: cancel,
      );
      _check(epoch, admission);
      final hasMore = boolField(data, 'has_more'),
          raw = data[kind == 'group' ? 'groups' : 'conversations'];
      if (raw is! List || raw.length > 50)
        throw const FormatException('会话列表无效');
      String? next;
      if (hasMore) {
        next = stringField(data, 'next_cursor', max: 256);
        if (next == cursor || RegExp(r'[\x00-\x20\x7f]').hasMatch(next))
          throw const FormatException('分页游标无效');
      } else if (data.containsKey('next_cursor')) {
        throw const FormatException('分页游标无效');
      }
      final items = <ConversationSummary>[],
          seen = <String>{},
          statuses = <String, String>{};
      for (final value in raw) {
        final row = objectMap(value);
        if (row['type'] != kind) throw const FormatException('会话类别无效');
        wireTime(row['updated_at']);
        if (kind == 'group') {
          final status = stringField(row, 'status', max: 32),
              role = stringField(row, 'role', max: 16);
          uuidField(row, 'source_membership_id');
          if (!const ['active', 'policy_blocked'].contains(status) ||
              !const ['owner', 'admin', 'member'].contains(role))
            throw const FormatException('群状态无效');
          statuses[uuidField(row, 'id')] = status;
        } else if (row['peer_visible'] == false &&
            (row.containsKey('display_name') ||
                row.containsKey('organization_name'))) {
          throw const FormatException('不可见联系人含个人信息');
        }
        final item = ConversationSummary.fromJson(row);
        if (!seen.add(item.id)) throw const FormatException('会话重复');
        items.add(item);
      }
      return ConversationPage._(
        List.unmodifiable(items),
        Map.unmodifiable(statuses),
        hasMore,
        next,
      );
    } finally {
      cancel.cancel();
      if (epoch == _epoch) _cancel = null;
    }
  }

  void close() {
    _closed = true;
    _epoch++;
    _cancel?.cancel();
    _cancel = null;
  }
}
