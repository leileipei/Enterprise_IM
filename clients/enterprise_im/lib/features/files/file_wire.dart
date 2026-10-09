import 'dart:async';
import 'dart:convert';
import 'dart:io';
import '../../core/protocol/api_client.dart';
import '../../core/protocol/models.dart';
import '../../core/sync/client_context.dart';

final class FileWireResponse {
  final int status;
  final Map<String, String> headers;
  final Stream<List<int>> body;
  const FileWireResponse(this.status, this.headers, this.body);
}

abstract interface class FileWireTransport {
  Future<ApiResponse> upload(
    ApiRequest request,
    Stream<List<int>> body,
    int length,
    Cancellation cancellation,
  );
  Future<FileWireResponse> download(
    ApiRequest request,
    Cancellation cancellation,
  );
  void close();
}

ApiRequest fileRequest(ClientContext ctx, String method, String id) {
  uuidField({'id': id}, 'id');
  return ApiRequest(
    ctx.session.origin.resolve('/api/v1/files/$id/content'),
    method,
    {
      'Authorization': 'Bearer ${ctx.session.accessToken}',
      'X-Acting-Membership-ID': ctx.actingMembershipId,
      'X-IM-Device-ID': ctx.device.deviceId,
      'X-IM-Device-Token': ctx.device.deviceToken,
      if (method == 'PUT') 'Content-Type': 'application/octet-stream',
    },
    null,
  );
}

Map<String, Object?> fileJSON(ApiResponse response) {
  if (response.status >= 300 && response.status < 400)
    throw const ApiFailure('redirect_rejected');
  Map<String, Object?> json;
  try {
    json = objectMap(jsonDecode(utf8.decode(response.body)));
  } catch (_) {
    throw const ApiFailure('invalid_response');
  }
  if (response.status < 200 || response.status >= 300) {
    final raw = json['error_code'];
    throw ApiFailure(
      raw is String && RegExp(r'^[a-z][a-z0-9_]{0,63}$').hasMatch(raw)
          ? raw
          : 'unavailable',
      status: response.status,
    );
  }
  return json;
}

final class HttpFileWireTransport implements FileWireTransport {
  final Uri origin;
  final HttpClient _client = HttpClient()
    ..connectionTimeout = const Duration(seconds: 10);
  HttpFileWireTransport(Uri origin) : origin = ApiClient.validateOrigin(origin);
  Future<HttpClientRequest> _open(ApiRequest r, Cancellation c) async {
    if (r.uri.origin != origin.origin ||
        !RegExp(
          r'^/api/v1/files/[0-9a-f-]{36}/content$',
        ).hasMatch(r.uri.path) ||
        r.uri.hasQuery ||
        r.uri.hasFragment)
      throw const ApiFailure('invalid_context');
    c.check();
    final req = await _client.openUrl(r.method, r.uri);
    if (c.isCancelled) {
      req.abort();
      c.check();
    }
    req.followRedirects = false;
    req.maxRedirects = 0;
    r.headers.forEach(req.headers.set);
    return req;
  }

  Map<String, String> _headers(HttpClientResponse r) {
    final map = <String, String>{};
    r.headers.forEach((k, v) => map[k.toLowerCase()] = v.join(','));
    return map;
  }

  @override
  Future<ApiResponse> upload(
    ApiRequest r,
    Stream<List<int>> body,
    int length,
    Cancellation c,
  ) async {
    HttpClientRequest? req;
    final finished = Completer<void>();
    unawaited(
      Future.any([c.whenCancelled, finished.future]).then((_) {
        if (c.isCancelled) req?.abort();
      }),
    );
    Future<ApiResponse> perform() async {
      req = await _open(r, c);
      req!.contentLength = length;
      await req!.addStream(body);
      c.check();
      final response = await req!.close();
      c.check();
      final bytes = <int>[];
      await for (final chunk in response) {
        c.check();
        if (bytes.length + chunk.length > 16384)
          throw const ApiFailure('response_too_large');
        bytes.addAll(chunk);
      }
      return ApiResponse(response.statusCode, bytes, _headers(response));
    }

    try {
      return await Future.any([
        perform(),
        c.whenCancelled.then<ApiResponse>(
          (_) => throw const ApiFailure('cancelled'),
        ),
      ]).timeout(
        const Duration(seconds: 150),
        onTimeout: () {
          c.cancel();
          throw const ApiFailure('timeout');
        },
      );
    } on ApiFailure {
      rethrow;
    } catch (_) {
      throw const ApiFailure('unavailable');
    } finally {
      req?.abort();
      finished.complete();
    }
  }

  @override
  Future<FileWireResponse> download(ApiRequest r, Cancellation c) async {
    HttpClientRequest? req;
    final finished = Completer<void>();
    final timer = Timer(const Duration(seconds: 60), c.cancel);
    unawaited(
      Future.any([c.whenCancelled, finished.future]).then((_) {
        if (c.isCancelled) {
          timer.cancel();
          req?.abort();
        }
      }),
    );
    try {
      Future<HttpClientResponse> connect() async {
        req = await _open(r, c);
        final response = await req!.close();
        c.check();
        return response;
      }

      final response = await Future.any([
        connect(),
        c.whenCancelled.then<HttpClientResponse>(
          (_) => throw const ApiFailure('cancelled'),
        ),
      ]);
      Stream<List<int>> chunks() async* {
        try {
          await for (final bytes in response) {
            c.check();
            yield bytes;
          }
          c.check();
        } finally {
          timer.cancel();
          req?.abort();
          if (!finished.isCompleted) finished.complete();
        }
      }

      return FileWireResponse(
        response.statusCode,
        _headers(response),
        chunks(),
      );
    } catch (_) {
      timer.cancel();
      req?.abort();
      finished.complete();
      if (c.isCancelled) throw const ApiFailure('cancelled');
      rethrow;
    }
  }

  @override
  void close() => _client.close(force: true);
}
