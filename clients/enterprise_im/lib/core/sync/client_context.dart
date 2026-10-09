import '../auth/auth_session.dart';
import '../protocol/models.dart';

final class ClientContext {
  final AuthSession session;
  final String actingMembershipId, conversationId;
  final DeviceGrant device;
  final int generation;
  ClientContext({
    required this.session,
    required this.actingMembershipId,
    required this.device,
    required this.generation,
    required this.conversationId,
  }) {
    uuidField({'id': actingMembershipId}, 'id');
    uuidField({'id': conversationId}, 'id');
    if (generation < 0 || generation != session.generation)
      throw const FormatException('账号代次无效');
  }
  bool sameScope(ClientContext other) =>
      session.origin == other.session.origin &&
      session.tenantId == other.session.tenantId &&
      session.userId == other.session.userId &&
      actingMembershipId == other.actingMembershipId &&
      device.deviceId == other.device.deviceId &&
      device.deviceToken == other.device.deviceToken &&
      generation == other.generation &&
      conversationId == other.conversationId;
  @override
  String toString() => 'ClientContext(generation: $generation)';
}
