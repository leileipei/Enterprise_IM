import '../../core/protocol/models.dart';

final class Membership {
  final String id,
      organizationId,
      organizationName,
      legalEntityId,
      legalEntityName,
      title;
  final bool isPrimary;
  Membership._(
    this.id,
    this.organizationId,
    this.organizationName,
    this.legalEntityId,
    this.legalEntityName,
    this.title,
    this.isPrimary,
  );
  factory Membership.fromJson(Map<String, Object?> data) => Membership._(
    uuidField(data, 'id'),
    uuidField(data, 'organization_id'),
    stringField(data, 'organization_name', max: 1024),
    uuidField(data, 'legal_entity_id'),
    stringField(data, 'legal_entity_name', max: 1024),
    stringField(data, 'title', max: 1024, empty: true),
    boolField(data, 'is_primary'),
  );
}

final class SelfContext {
  final String tenantId, userId, displayName;
  final List<Membership> memberships;
  SelfContext._(this.tenantId, this.userId, this.displayName, this.memberships);
  factory SelfContext.fromJson(Map<String, Object?> data) {
    final rows = data['memberships'];
    if (rows is! List || rows.length > 10000)
      throw const FormatException('任职响应无效');
    stringField(data, 'global_employee_no', empty: true);
    final seen = <String>{}, memberships = <Membership>[];
    for (final raw in rows) {
      final member = Membership.fromJson(objectMap(raw));
      if (!seen.add(member.id)) throw const FormatException('任职响应重复');
      memberships.add(member);
    }
    return SelfContext._(
      uuidField(data, 'tenant_id'),
      uuidField(data, 'user_id'),
      stringField(data, 'display_name', max: 1024),
      List.unmodifiable(memberships),
    );
  }
}
