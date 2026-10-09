import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data';
import 'api_error.dart';
import 'models.dart';
export 'api_error.dart';

final class Cancellation {
  final _cancelled = Completer<void>();
  bool get isCancelled => _cancelled.isCompleted;
  Future<void> get whenCancelled => _cancelled.future;
  void cancel() {
    if (!isCancelled) _cancelled.complete();
  }

  void check() {
    if (isCancelled) throw const ApiFailure('cancelled');
  }
}

final class ApiRequest {
  final Uri uri;
  final String method;
  final Map<String, String> headers;
  final List<int>? body;
  final int responseByteLimit;
  ApiRequest(
    this.uri,
    this.method,
    Map<String, String> headers,
    this.body, {
    this.responseByteLimit = 2 * 1024 * 1024,
  }) : headers = Map.unmodifiable(headers);
}

final class ApiResponse {
  final int status;
  final List<int> body;
  final Map<String, String> headers;
  const ApiResponse(this.status, this.body, this.headers);
}

abstract interface class ApiTransport {
  Future<ApiResponse> send(ApiRequest request, Cancellation cancel);
  void close();
}

final class HttpApiTransport implements ApiTransport {
  final HttpClient _client = HttpClient()
    ..connectionTimeout = const Duration(seconds: 10);
  @override
  Future<ApiResponse> send(ApiRequest request, Cancellation cancel) async {
    cancel.check();
    HttpClientRequest? pending;
    final finished = Completer<void>();
    unawaited(
      Future.any([cancel.whenCancelled, finished.future]).then((_) {
        if (cancel.isCancelled) pending?.abort(const ApiFailure('cancelled'));
      }),
    );
    try {
      pending = await _client.openUrl(request.method, request.uri);
      cancel.check();
      pending.followRedirects = false;
      pending.maxRedirects = 0;
      request.headers.forEach(pending.headers.set);
      if (request.body != null) pending.add(request.body!);
      final response = await pending.close();
      cancel.check();
      final body = BytesBuilder(copy: false);
      await for (final chunk in response) {
        cancel.check();
        if (body.length + chunk.length > request.responseByteLimit) {
          pending.abort();
          throw const ApiFailure('response_too_large');
        }
        body.add(chunk);
      }
      final headers = <String, String>{};
      response.headers.forEach((name, values) {
        headers[name.toLowerCase()] = values.join(',');
      });
      return ApiResponse(response.statusCode, body.takeBytes(), headers);
    } on ApiFailure {
      rethrow;
    } catch (_) {
      throw const ApiFailure('unavailable');
    } finally {
      if (!finished.isCompleted) finished.complete();
    }
  }

  @override
  void close() => _client.close(force: true);
}

final class ApiClient {
  final Uri origin;
  final ApiTransport _transport;
  ApiClient(Uri origin, ApiTransport transport)
    : origin = validateOrigin(origin),
      _transport = transport;
  static Uri validateOrigin(Uri value) {
    if (value.scheme != 'https' ||
        value.host.isEmpty ||
        value.userInfo.isNotEmpty ||
        value.hasQuery ||
        value.hasFragment ||
        (value.path.isNotEmpty && value.path != '/'))
      throw const FormatException('服务地址必须为 HTTPS 根地址');
    if (value.port < 1 || value.port > 65535)
      throw const FormatException('服务端口无效');
    return value.replace(path: '');
  }

  Future<Map<String, Object?>> json(
    String method,
    String path, {
    Map<String, Object?>? body,
    String? bearer,
    String? actingMembershipId,
    DeviceGrant? device,
    Cancellation? cancellation,
    Duration timeout = const Duration(seconds: 15),
  }) async {
    final relative = Uri.parse(path);
    if (relative.hasScheme ||
        relative.hasAuthority ||
        !path.startsWith('/api/v1/') ||
        relative.hasFragment)
      throw const FormatException('请求地址无效');
    final uri = origin.resolveUri(relative);
    if (uri.origin != origin.origin) throw const FormatException('请求来源无效');
    final responseByteLimit =
        RegExp(
          r'^/api/v1/conversations/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/sync$',
        ).hasMatch(uri.path)
        ? 64 * 1024 * 1024
        : 2 * 1024 * 1024;
    final cancel = cancellation ?? Cancellation();
    cancel.check();
    final headers = <String, String>{'Accept': 'application/json'};
    if (body != null) headers['Content-Type'] = 'application/json';
    if (bearer != null) {
      if (bearer.isEmpty ||
          bearer.length > 8192 ||
          RegExp(r'[\x00-\x20\x7f]').hasMatch(bearer))
        throw const FormatException('认证凭据无效');
      headers['Authorization'] = 'Bearer $bearer';
    }
    if (actingMembershipId != null) {
      uuidField({'id': actingMembershipId}, 'id');
      headers['X-Acting-Membership-ID'] = actingMembershipId;
    }
    if (device != null) {
      uuidField({'id': device.deviceId}, 'id');
      if (!proofValid(device.deviceToken))
        throw const FormatException('设备凭据无效');
      headers['X-IM-Device-ID'] = device.deviceId;
      headers['X-IM-Device-Token'] = device.deviceToken;
    }
    try {
      final response =
          await Future.any<ApiResponse>([
            _transport.send(
              ApiRequest(
                uri,
                method,
                headers,
                body == null ? null : utf8.encode(jsonEncode(body)),
                responseByteLimit: responseByteLimit,
              ),
              cancel,
            ),
            cancel.whenCancelled.then<ApiResponse>(
              (_) => throw const ApiFailure('cancelled'),
            ),
          ]).timeout(
            timeout,
            onTimeout: () {
              cancel.cancel();
              throw const ApiFailure('timeout');
            },
          );
      cancel.check();
      if (response.body.length > responseByteLimit)
        throw const ApiFailure('response_too_large');
      if (response.status >= 300 && response.status < 400)
        throw const ApiFailure('redirect_rejected');
      Object? decoded;
      try {
        decoded = jsonDecode(utf8.decode(response.body));
      } catch (_) {
        throw const ApiFailure('invalid_response');
      }
      if (decoded is! Map<String, Object?>)
        throw const ApiFailure('invalid_response');
      if (response.status < 200 || response.status >= 300) {
        final raw = decoded['error_code'];
        final code =
            raw is String && RegExp(r'^[a-z][a-z0-9_]{0,63}$').hasMatch(raw)
            ? raw
            : 'unavailable';
        final seconds = int.tryParse(response.headers['retry-after'] ?? '');
        throw ApiFailure(
          code,
          status: response.status,
          retryAfter: seconds != null && seconds > 0 && seconds <= 86400
              ? Duration(seconds: seconds)
              : null,
        );
      }
      return decoded;
    } on ApiFailure {
      rethrow;
    } on FormatException {
      throw const ApiFailure('invalid_response');
    } catch (_) {
      throw const ApiFailure('unavailable');
    } finally {
      if (cancellation == null) cancel.cancel();
    }
  }

  void close() => _transport.close();
}
