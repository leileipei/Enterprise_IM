import '../../core/protocol/models.dart';
import '../memberships/membership_controller.dart';
import '../memberships/workspace_requests.dart';

final class DirectoryRole {
  final String id, organizationName, title;
  final List<String> departments;
  DirectoryRole._(this.id, this.organizationName, this.title, this.departments);
  factory DirectoryRole.fromJson(Map<String, Object?> data) {
    uuidField(data, 'organization_id');
    boolField(data, 'is_primary');
    final rows = data['departments'];
    if (rows is! List || rows.length > 10000)
      throw const FormatException('部门列表无效');
    final ids = <String>{}, names = <String>[];
    for (final raw in rows) {
      final row = objectMap(raw);
      if (!ids.add(uuidField(row, 'id'))) throw const FormatException('重复部门');
      names.add(stringField(row, 'name', max: 1024));
    }
    return DirectoryRole._(
      uuidField(data, 'membership_id'),
      stringField(data, 'organization_name', max: 1024),
      stringField(data, 'title', max: 1024, empty: true),
      List.unmodifiable(names),
    );
  }
}

final class DirectoryPerson {
  final String id, name, employeeNo;
  final List<DirectoryRole> memberships;
  DirectoryPerson._(this.id, this.name, this.employeeNo, this.memberships);
  factory DirectoryPerson.fromJson(Map<String, Object?> data) {
    final raw = data['memberships'];
    if (raw is! List || raw.isEmpty || raw.length > 10000)
      throw const FormatException('可见任职无效');
    final ids = <String>{}, roles = <DirectoryRole>[];
    for (final row in raw) {
      final role = DirectoryRole.fromJson(objectMap(row));
      if (!ids.add(role.id)) throw const FormatException('重复任职');
      roles.add(role);
    }
    return DirectoryPerson._(
      uuidField(data, 'id'),
      stringField(data, 'display_name', max: 1024),
      stringField(data, 'employee_no', max: 128, empty: true),
      List.unmodifiable(roles),
    );
  }
}

final class PeoplePage {
  final List<DirectoryPerson> people;
  final bool hasMore;
  PeoplePage._(this.people, this.hasMore);
  factory PeoplePage.fromJson(Map<String, Object?> data) {
    final raw = data['people'];
    if (raw is! List || raw.length > 20) throw const FormatException('通讯录响应无效');
    final ids = <String>{}, people = <DirectoryPerson>[];
    for (final row in raw) {
      final person = DirectoryPerson.fromJson(objectMap(row));
      if (!ids.add(person.id)) throw const FormatException('重复人员');
      people.add(person);
    }
    return PeoplePage._(List.unmodifiable(people), boolField(data, 'has_more'));
  }
}

final class DirectoryController {
  final WorkspaceRequests requests;
  DirectoryController(MembershipController memberships)
    : requests = WorkspaceRequests(memberships);
  Future<PeoplePage> search(String query) async {
    final text = query.trim();
    if (text.runes.length < 2 ||
        text.runes.length > 100 ||
        text.contains('\u0000'))
      throw const FormatException('搜索内容需2至100个字符');
    final data = await requests.request(
      'GET',
      Uri(
        path: '/api/v1/directory/users',
        queryParameters: {'q': text, 'limit': '20'},
      ).toString(),
    );
    return PeoplePage.fromJson(data);
  }

  void close() => requests.close();
}
