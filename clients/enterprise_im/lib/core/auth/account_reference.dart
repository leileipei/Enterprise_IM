import '../protocol/api_client.dart';
import '../protocol/models.dart';
import 'auth_session.dart';

// An account selector is a reference, never proof of authenticated identity.
// Opening the database still requires a freshly verified AuthSession.
final class AccountReference {
  final Uri origin;
  final String tenantId, userId;
  AccountReference({
    required Uri origin,
    required this.tenantId,
    required this.userId,
  }) : origin = ApiClient.validateOrigin(origin) {
    uuidField({'id': tenantId}, 'id');
    uuidField({'id': userId}, 'id');
  }
  factory AccountReference.fromSession(AuthSession session) => AccountReference(
    origin: session.origin,
    tenantId: session.tenantId,
    userId: session.userId,
  );
  @override
  String toString() => 'AccountReference';
}
