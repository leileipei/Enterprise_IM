import 'dart:async';
import 'dart:io';
import 'package:flutter/services.dart';
import 'package:file_selector/file_selector.dart' as selector;
import '../core/protocol/api_client.dart';

abstract interface class FileExporter {
  Future<bool> save(
    String filename,
    Stream<List<int>> Function() contents,
    Cancellation cancel,
  );
}

// Choosing a destination precedes the contents callback; the caller performs
// fresh authorization there. The private business path never crosses the bridge.
final class FileExport implements FileExporter {
  final MethodChannel _channel;
  final String _platform;
  FileExport({MethodChannel? channel, String? platform})
    : _channel = channel ?? const MethodChannel('enterprise_im/file_export'),
      _platform = platform ?? Platform.operatingSystem;
  @override
  Future<bool> save(
    String filename,
    Stream<List<int>> Function() contents,
    Cancellation cancel,
  ) async {
    cancel.check();
    if (_platform == 'android') {
      final pending = _channel.invokeMethod<String>('beginExport', {
        'name': filename,
      });
      unawaited(
        pending
            .then((ticket) async {
              if (cancel.isCancelled && ticket != null)
                await _channel.invokeMethod<void>('abortExport', {
                  'ticket': ticket,
                });
            })
            .catchError((Object _) {}),
      );
      final ticket = await Future.any([
        pending,
        cancel.whenCancelled.then<String?>(
          (_) => throw const ApiFailure('cancelled'),
        ),
      ]);
      if (ticket == null) return false;
      try {
        cancel.check();
        await for (final chunk in contents()) {
          cancel.check();
          for (var start = 0; start < chunk.length; start += 32768) {
            cancel.check();
            final end = start + 32768 < chunk.length
                ? start + 32768
                : chunk.length;
            await _channel.invokeMethod<void>('writeExport', {
              'ticket': ticket,
              'bytes': Uint8List.fromList(chunk.sublist(start, end)),
            });
          }
        }
        cancel.check();
        await _channel.invokeMethod<void>('finishExport', {'ticket': ticket});
        return true;
      } catch (_) {
        await _channel.invokeMethod<void>('abortExport', {'ticket': ticket});
        rethrow;
      }
    }
    if (_platform == 'ios') throw const ApiFailure('native_export_unavailable');
    final choice = await Future.any([
      selector.getSaveLocation(suggestedName: filename),
      cancel.whenCancelled.then<selector.FileSaveLocation?>(
        (_) => throw const ApiFailure('cancelled'),
      ),
    ]);
    cancel.check();
    if (choice == null) return false;
    final target = File(choice.path);
    final staging = await target.parent.createTemp('.enterprise-im-export-');
    final temp = File('${staging.path}/content');
    RandomAccessFile? output;
    try {
      output = await temp.open(mode: FileMode.write);
      await for (final chunk in contents()) {
        cancel.check();
        await output.writeFrom(chunk);
      }
      await output.flush();
      await output.close();
      output = null;
      cancel.check();
      // System save chooser confirms replacement. POSIX rename commits only a
      // fully copied file; failed copy never truncates the existing destination.
      await temp.rename(target.path);
      return true;
    } finally {
      await output?.close();
      if (await staging.exists()) await staging.delete(recursive: true);
    }
  }
}
