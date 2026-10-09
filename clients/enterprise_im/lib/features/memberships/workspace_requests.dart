import '../../core/protocol/api_client.dart';
import 'membership_controller.dart';

final class WorkspaceRequests {
  final MembershipController memberships;
  final _cancellations = <Cancellation>{};
  int _epoch = 0;
  bool _closed = false;
  WorkspaceRequests(this.memberships);
  WorkspaceAdmission get ready {
    final value = memberships.current;
    if (_closed || value == null) throw const ApiFailure('reauthenticate');
    return value;
  }

  void check(WorkspaceAdmission admission, int epoch) {
    if (_closed ||
        epoch != _epoch ||
        !identical(memberships.current, admission))
      throw const ApiFailure('cancelled');
  }

  int get epoch => _epoch;
  Future<Map<String, Object?>> request(
    String method,
    String path, {
    Map<String, Object?>? body,
  }) async {
    final admission = ready, epoch = _epoch, cancel = Cancellation();
    _cancellations.add(cancel);
    try {
      final result = await memberships.owner.auth.api.json(
        method,
        path,
        bearer: admission.client.session.accessToken,
        actingMembershipId: admission.membership.id,
        device: admission.device,
        body: body,
        cancellation: cancel,
      );
      check(admission, epoch);
      return result;
    } finally {
      cancel.cancel();
      _cancellations.remove(cancel);
    }
  }

  void close() {
    _closed = true;
    _epoch++;
    for (final cancel in _cancellations) {
      cancel.cancel();
    }
    _cancellations.clear();
  }
}
