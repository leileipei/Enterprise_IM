import 'dart:convert';
import '../storage/secure_vault.dart';
import '../protocol/models.dart';
import 'account_reference.dart';

// The single last-account reference is OS-vault metadata, not authentication.
// A provider-scoped queue prevents old removal overtaking later selection.
final class AccountSelector {
  final SecureVault vault;
  static final _tails = Expando<Future<void>>('account-selection');
  final Object _scope;
  AccountSelector(SecureVault provider)
    : vault = provider,
      _scope = provider is CoordinatedVault
          ? (provider as CoordinatedVault).coordinationScope
          : provider;
  Future<T> _serial<T>(Future<T> Function() operation) {
    final tail = _tails[_scope] ?? Future<void>.value();
    final result = tail.then((_) => operation());
    _tails[_scope] = result.then<void>(
      (_) {},
      onError: (Object _, StackTrace __) {},
    );
    return result;
  }

  Future<AccountReference?> load() => _serial(_load);
  Future<AccountReference?> _load() async {
    final raw = await vault.read(bootstrapNamespace, 'account');
    if (raw == null) return null;
    try {
      if (utf8.encode(raw).length > 4096) throw const FormatException();
      final data = objectMap(jsonDecode(raw));
      if (data.length != 4 || data['version'] != 1)
        throw const FormatException();
      return AccountReference(
        origin: Uri.parse(stringField(data, 'origin', max: 2048)),
        tenantId: uuidField(data, 'tenant_id'),
        userId: uuidField(data, 'user_id'),
      );
    } catch (_) {
      throw const VaultFailure();
    }
  }

  Future<void> save(AccountReference account) => _serial(
    () => vault.write(
      bootstrapNamespace,
      'account',
      jsonEncode({
        'version': 1,
        'origin': account.origin.origin,
        'tenant_id': account.tenantId,
        'user_id': account.userId,
      }),
    ),
  );
  Future<void> forget(AccountReference account) => _serial(() async {
    final saved = await _load();
    if (saved != null &&
        saved.origin == account.origin &&
        saved.tenantId == account.tenantId &&
        saved.userId == account.userId)
      await vault.delete(bootstrapNamespace, 'account');
  });
}
