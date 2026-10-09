import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:enterprise_im/core/protocol/api_client.dart';
import 'package:enterprise_im/core/sync/realtime_channel.dart';

final class LocalHttpOverrides extends HttpOverrides {}

void main() {
  late Directory root;
  late SecurityContext serverContext, clientContext;
  final ticket = base64Url.encode(List<int>.filled(32, 6)).replaceAll('=', '');
  setUpAll(() async {
    root = await Directory.systemTemp.createTemp('im-wss-fixture-');
    final caConfig = File('${root.path}/ca.conf');
    await caConfig.writeAsString(
      '[req]\ndistinguished_name=dn\nx509_extensions=ext\nprompt=no\n[dn]\nCN=Enterprise IM Local Test CA\n[ext]\nbasicConstraints=critical,CA:true\nkeyUsage=critical,keyCertSign,cRLSign\nsubjectKeyIdentifier=hash\n',
    );
    final leafConfig = File('${root.path}/leaf.conf');
    await leafConfig.writeAsString(
      '[req]\ndistinguished_name=dn\nprompt=no\n[dn]\nCN=localhost\n[ext]\nsubjectAltName=DNS:localhost,IP:127.0.0.1\nbasicConstraints=critical,CA:false\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid,issuer\n',
    );
    for (final args in [
      [
        'req',
        '-x509',
        '-sha256',
        '-newkey',
        'rsa:2048',
        '-nodes',
        '-days',
        '1',
        '-config',
        caConfig.path,
        '-keyout',
        '${root.path}/ca-key.pem',
        '-out',
        '${root.path}/ca.pem',
      ],
      [
        'req',
        '-sha256',
        '-newkey',
        'rsa:2048',
        '-nodes',
        '-config',
        leafConfig.path,
        '-keyout',
        '${root.path}/key.pem',
        '-out',
        '${root.path}/leaf.csr',
      ],
      [
        'x509',
        '-req',
        '-sha256',
        '-days',
        '1',
        '-in',
        '${root.path}/leaf.csr',
        '-CA',
        '${root.path}/ca.pem',
        '-CAkey',
        '${root.path}/ca-key.pem',
        '-CAcreateserial',
        '-extfile',
        leafConfig.path,
        '-extensions',
        'ext',
        '-out',
        '${root.path}/cert.pem',
      ],
    ]) {
      final generated = await Process.run('/usr/bin/openssl', args);
      expect(
        generated.exitCode,
        0,
        reason: 'local TLS fixture generation failed',
      );
    }
    serverContext = SecurityContext()
      ..useCertificateChain('${root.path}/cert.pem')
      ..usePrivateKey('${root.path}/key.pem');
    clientContext = SecurityContext(withTrustedRoots: false)
      ..setTrustedCertificates('${root.path}/ca.pem');
  });
  tearDownAll(() async {
    await root.delete(recursive: true);
  });
  test('real WSS upgrade sends ticket only as subprotocol', () async {
    final server = await HttpServer.bindSecure(
      InternetAddress.loopbackIPv4,
      0,
      serverContext,
    );
    final observed = Completer<HttpRequest>();
    WebSocket? peer;
    final sub = server.listen((req) async {
      observed.complete(req);
      peer = await WebSocketTransformer.upgrade(
        req,
        protocolSelector: (protocols) =>
            protocols.contains('enterprise-im.v1') ? 'enterprise-im.v1' : null,
      );
      peer!.add('{"type":"ready","resync_required":true}');
    });
    addTearDown(() async {
      await peer?.close();
      await sub.cancel();
      await server.close(force: true);
    });
    final connector = HttpRealtimeConnector(
      createClient: () => LocalHttpOverrides().createHttpClient(clientContext),
    );
    final cancel = Cancellation();
    final socket = await connector.connect(
      Uri.parse('wss://127.0.0.1:${server.port}/api/v1/realtime'),
      ['enterprise-im.v1', 'ticket.$ticket'],
      cancel,
    );
    addTearDown(socket.close);
    final request = await observed.future;
    expect(request.uri.toString(), '/api/v1/realtime');
    expect(request.headers.value('authorization'), isNull);
    expect(
      request.headers.value('sec-websocket-protocol'),
      'enterprise-im.v1, ticket.$ticket',
    );
    expect(socket.protocol, 'enterprise-im.v1');
    expect(
      await socket.frames.first,
      '{"type":"ready","resync_required":true}',
    );
  });
  test('real cross origin redirect never receives ticket', () async {
    final target = await HttpServer.bindSecure(
      InternetAddress.loopbackIPv4,
      0,
      serverContext,
    );
    var leakedRequests = 0;
    final targetSub = target.listen((req) async {
      leakedRequests++;
      req.response.statusCode = 403;
      await req.response.close();
    });
    final source = await HttpServer.bindSecure(
      InternetAddress.loopbackIPv4,
      0,
      serverContext,
    );
    final sourceSub = source.listen((req) async {
      req.response.statusCode = 302;
      req.response.headers.set(
        'location',
        'https://127.0.0.1:${target.port}/api/v1/realtime',
      );
      await req.response.close();
    });
    addTearDown(() async {
      await sourceSub.cancel();
      await targetSub.cancel();
      await source.close(force: true);
      await target.close(force: true);
    });
    final connector = HttpRealtimeConnector(
      createClient: () => LocalHttpOverrides().createHttpClient(clientContext),
    );
    await expectLater(
      connector.connect(
        Uri.parse('wss://127.0.0.1:${source.port}/api/v1/realtime'),
        ['enterprise-im.v1', 'ticket.$ticket'],
        Cancellation(),
      ),
      throwsA(
        isA<ApiFailure>().having((e) => e.code, 'code', 'redirect_rejected'),
      ),
    );
    expect(leakedRequests, 0);
  });
}
