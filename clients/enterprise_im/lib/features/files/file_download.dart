import 'dart:async';
import 'dart:convert';
import 'dart:io';
import '../../core/protocol/api_client.dart';
import '../../core/protocol/models.dart';
import '../../core/protocol/seq.dart';
import '../../core/storage/records.dart';
import '../../core/storage/secure_vault.dart';
import '../../core/sync/client_context.dart';
import '../../platform/file_export.dart';
import '../memberships/membership_controller.dart';
import 'file_wire.dart';

final class DownloadedArtifact {
  final ClientContext _context;
  final MessageDTO _message;
  final Duration Function() _remaining;
  final Duration _deadline;
  final File _file;
  final Directory _directory;
  final String name;
  final int size;
  Timer? _expiry;
  Future<void>? _erasing;
  bool _deleted = false;
  DownloadedArtifact._(
    this._context,
    this._message,
    this._remaining,
    this._deadline,
    this._file,
    this._directory,
    this.name,
    this.size,
  );
}

final class FileDownload {
  final MembershipController memberships;
  final FileWireTransport wire;
  final FileExporter exporter;
  final Directory? privateRoot;
  final Set<DownloadedArtifact> _artifacts = {};
  final Set<Directory> _staging = {};
  late final WorkspaceAdmission _admission;
  late final void Function() _detach;
  Cancellation? _cancel, _exportCancel;
  Duration Function()? _permission;
  Future<Object?>? _flight;
  Future<void>? _closing;
  bool _closed = false, _exporting = false;
  FileDownload(
    this.memberships, {
    FileWireTransport? wire,
    FileExporter? exporter,
    this.privateRoot,
  }) : wire = wire ?? HttpFileWireTransport(memberships.owner.auth.api.origin),
       exporter = exporter ?? FileExport() {
    final ready = memberships.current;
    if (ready == null) throw const ApiFailure('invalid_context');
    _admission = ready;
    _detach = memberships.owner.sessions.ownPrivateFiles(
      ready.client.session,
      close,
    );
  }
  void _check(
    ClientContext ctx,
    Duration Function() remaining, {
    Duration? deadline,
  }) {
    if (_closed ||
        !identical(memberships.current, _admission) ||
        !identical(ctx.session, _admission.client.session) ||
        ctx.actingMembershipId != _admission.membership.id ||
        ctx.device.deviceId != _admission.device.deviceId ||
        ctx.device.deviceToken != _admission.device.deviceToken ||
        ctx.generation != _admission.client.store.generation ||
        remaining() <= Duration.zero ||
        (deadline != null && LeaseClock.monotonicNow() >= deadline))
      throw const ApiFailure('file_authorization_expired');
  }

  Future<DownloadedArtifact> downloadAuthorized(
    ClientContext ctx,
    MessageDTO message, {
    required Duration Function() authorizationRemaining,
  }) async {
    if (_flight != null) throw const ApiFailure('download_in_progress');
    final flight = _download(ctx, message, authorizationRemaining);
    _flight = flight;
    try {
      return await flight;
    } finally {
      if (identical(_flight, flight)) _flight = null;
    }
  }

  Future<DownloadedArtifact> _download(
    ClientContext ctx,
    MessageDTO message,
    Duration Function() remaining,
  ) async {
    _check(ctx, remaining);
    final card = message.attachment;
    if (message.redacted ||
        message.messageType != 'file' ||
        card?['available'] != true)
      throw const ApiFailure('file_not_available');
    final id = uuidField(card!, 'file_id'),
        name = stringField(card, 'original_filename', max: 255),
        size = Seq.fromWire(card['actual_size_bytes']).value.toInt();
    if (size < 1 ||
        size > 25 * 1024 * 1024 ||
        utf8.encode(name).length > 255 ||
        name.trim() != name ||
        name == '.' ||
        name == '..' ||
        RegExp(r'[\x00-\x1f\x7f-\x9f/\\:]').hasMatch(name))
      throw const ApiFailure('invalid_response');
    var budget = remaining();
    for (final limit in [ctx.session.remaining, _admission.remaining]) {
      if (limit < budget) budget = limit;
    }
    final deadline = LeaseClock.monotonicNow() + budget;
    final cancel = Cancellation();
    _cancel = cancel;
    _permission = remaining;
    final timer = Timer(
      budget < const Duration(seconds: 60)
          ? budget
          : const Duration(seconds: 60),
      cancel.cancel,
    );
    Directory? dir;
    RandomAccessFile? output;
    bool keep = false;
    try {
      // Assembly capability is advisory, never a grant for this file. The
      // content GET below reauthorizes against current policy and device state.
      final caps = await memberships.owner.auth.api.json(
        'GET',
        '/api/v1/file-capabilities',
        bearer: ctx.session.accessToken,
        actingMembershipId: ctx.actingMembershipId,
        device: ctx.device,
        cancellation: cancel,
      );
      _check(ctx, remaining, deadline: deadline);
      cancel.check();
      const keys = [
        'upload_enabled',
        'message_send_enabled',
        'download_enabled',
        'filename_search_enabled',
      ];
      if (caps.length != keys.length || keys.any((k) => caps[k] is! bool))
        throw const ApiFailure('invalid_response');
      if (caps['download_enabled'] != true)
        throw const ApiFailure('file_download_disabled');
      final ns = await accountNamespace(
        ctx.session.origin,
        ctx.session.tenantId,
        ctx.session.userId,
      );
      _check(ctx, remaining, deadline: deadline);
      final base =
          privateRoot ??
          Directory(
            '${memberships.owner.sessions.directory.path}/$ns.attachments',
          );
      await base.create(recursive: true);
      _check(ctx, remaining, deadline: deadline);
      dir = await base.createTemp('artifact-');
      _staging.add(dir);
      _check(ctx, remaining, deadline: deadline);
      if (Platform.isMacOS || Platform.isLinux) {
        final mode = await Process.run('/bin/chmod', ['700', dir.path]);
        if (mode.exitCode != 0)
          throw const ApiFailure('private_storage_unavailable');
      }
      final file = File('${dir.path}/content');
      output = await file.open(mode: FileMode.write);
      _check(ctx, remaining, deadline: deadline);
      final response = await Future.any([
        wire.download(fileRequest(ctx, 'GET', id), cancel),
        cancel.whenCancelled.then<FileWireResponse>(
          (_) => throw const ApiFailure('cancelled'),
        ),
      ]);
      _check(ctx, remaining, deadline: deadline);
      cancel.check();
      if (response.status != 200) {
        if (response.status >= 300 && response.status < 400)
          throw const ApiFailure('redirect_rejected');
        final bytes = <int>[];
        await for (final chunk in response.body) {
          cancel.check();
          if (bytes.length + chunk.length > 16384)
            throw const ApiFailure('invalid_response');
          bytes.addAll(chunk);
        }
        fileJSON(ApiResponse(response.status, bytes, response.headers));
        throw const ApiFailure('invalid_response');
      }
      if (response.headers['content-type'] != 'application/octet-stream' ||
          response.headers['content-length'] != size.toString() ||
          response.headers['cache-control'] != 'no-store')
        throw const ApiFailure('invalid_response');
      var written = 0;
      final chunks = StreamIterator(response.body);
      try {
        while (await Future.any([
          chunks.moveNext(),
          cancel.whenCancelled.then<bool>(
            (_) => throw const ApiFailure('cancelled'),
          ),
        ])) {
          cancel.check();
          _check(ctx, remaining, deadline: deadline);
          final chunk = chunks.current;
          if (written + chunk.length > size)
            throw const ApiFailure('invalid_response');
          await output.writeFrom(chunk);
          written += chunk.length;
          cancel.check();
          _check(ctx, remaining, deadline: deadline);
        }
      } finally {
        await chunks.cancel();
      }
      _check(ctx, remaining, deadline: deadline);
      cancel.check();
      if (written != size) throw const ApiFailure('download_interrupted');
      await output.flush();
      await output.close();
      output = null;
      _check(ctx, remaining, deadline: deadline);
      final artifact = DownloadedArtifact._(
        ctx,
        message,
        remaining,
        deadline,
        file,
        dir,
        name,
        size,
      );
      _artifacts.add(artifact);
      _staging.remove(dir);
      keep = true;
      artifact._expiry = Timer(
        deadline - LeaseClock.monotonicNow(),
        () => unawaited(_erase(artifact).catchError((Object _) {})),
      );
      return artifact;
    } on ApiFailure {
      rethrow;
    } catch (_) {
      throw const ApiFailure('file_download_unavailable');
    } finally {
      timer.cancel();
      cancel.cancel();
      if (identical(_cancel, cancel)) {
        _cancel = null;
        _permission = null;
      }
      await output?.close();
      if (!keep && dir != null) {
        if (await dir.exists()) await dir.delete(recursive: true);
        _staging.remove(dir);
      }
    }
  }

  Future<void> _erase(DownloadedArtifact a) {
    if (a._erasing != null) return a._erasing!;
    a._expiry?.cancel();
    a._deleted = true;
    return a._erasing = () async {
      try {
        if (await a._directory.exists())
          await a._directory.delete(recursive: true);
        _artifacts.remove(a);
      } finally {
        a._erasing = null;
      }
    }();
  }

  Future<bool> exportPrivateFile(
    ClientContext ctx,
    DownloadedArtifact artifact,
  ) async {
    if (_exporting ||
        !_artifacts.contains(artifact) ||
        artifact._deleted ||
        !ctx.sameScope(artifact._context))
      throw const ApiFailure('invalid_context');
    _check(ctx, artifact._remaining, deadline: artifact._deadline);
    _exporting = true;
    final cancel = Cancellation();
    _exportCancel = cancel;
    final timer = Timer(
      artifact._deadline - LeaseClock.monotonicNow(),
      cancel.cancel,
    );
    try {
      Stream<List<int>> contents() async* {
        cancel.check();
        _check(ctx, artifact._remaining, deadline: artifact._deadline);
        // Destination selection may have changed policy or kept the dialog open.
        // Obtain a new server-authorized stream rather than export the old spool.
        final fresh = await downloadAuthorized(
          ctx,
          artifact._message,
          authorizationRemaining: artifact._remaining,
        );
        try {
          await for (final chunk in fresh._file.openRead()) {
            cancel.check();
            _check(ctx, artifact._remaining, deadline: artifact._deadline);
            yield chunk;
          }
          cancel.check();
          _check(ctx, artifact._remaining, deadline: artifact._deadline);
        } finally {
          await _erase(fresh);
        }
      }

      return await Future.any([
        exporter.save(artifact.name, contents, cancel),
        cancel.whenCancelled.then<bool>(
          (_) => throw const ApiFailure('cancelled'),
        ),
      ]);
    } finally {
      timer.cancel();
      cancel.cancel();
      _exporting = false;
      if (identical(_exportCancel, cancel)) _exportCancel = null;
    }
  }

  Future<void> prune() async {
    if ((_permission?.call() ?? const Duration(days: 1)) <= Duration.zero)
      _cancel?.cancel();
    for (final a in _artifacts.toList()) {
      if (a._remaining() <= Duration.zero ||
          LeaseClock.monotonicNow() >= a._deadline ||
          !identical(memberships.current, _admission))
        await _erase(a);
    }
  }

  Future<void> close() {
    if (_closing != null) return _closing!;
    _closed = true;
    _cancel?.cancel();
    _exportCancel?.cancel();
    wire.close();
    return _closing = () async {
      try {
        await _flight;
      } catch (_) {
        /* Request failure is already returned to its caller. */
      }
      for (final a in _artifacts.toList()) {
        await _erase(a);
      }
      for (final d in _staging.toList()) {
        if (await d.exists()) await d.delete(recursive: true);
        _staging.remove(d);
      }
      _detach();
    }();
  }
}
