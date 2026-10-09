import 'dart:convert';
import 'dart:io';
import 'dart:isolate';
import 'dart:typed_data';
import 'package:cryptography/cryptography.dart';
import 'package:sqlite3/sqlite3.dart';
import '../protocol/seq.dart';

Object? canonical(Object? value) {
  if (value is Map<String, Object?>) {
    final keys = value.keys.toList()..sort();
    return {for (final key in keys) key: canonical(value[key])};
  }
  if (value is List) return value.map(canonical).toList(growable: false);
  return value;
}

// All SQLite access and encryption is serial inside this one isolate. No other
// isolate obtains the database handle or executes a concurrent transaction.
Future<void> storeWorkerMain(List<Object?> args) async {
  final startup = args[0]! as SendPort;
  final replies = args[1]! as SendPort;
  final path = args[2]! as String;
  final namespace = args[3]! as String;
  final bytes = args[4]! as List<int>;
  final generation = args[5]! as int;
  final key = SecretKeyData(bytes, overwriteWhenDestroyed: true);
  bool keyDestroyed = false;
  void eraseKey() {
    if (!keyDestroyed) {
      key.destroy();
      keyDestroyed = true;
    }
    bytes.fillRange(0, bytes.length, 0);
  }

  final aes = AesGcm.with256bits();
  final commands = ReceivePort();
  Database? database;
  Future<String> opaque(String kind, String id) async {
    final hash = await Sha256().hash(
      utf8.encode(jsonEncode([namespace, kind, id])),
    );
    return hash.bytes
        .map((byte) => byte.toRadixString(16).padLeft(2, '0'))
        .join();
  }

  List<int> aad(String kind, String id) =>
      utf8.encode(jsonEncode([1, namespace, kind, id]));
  Future<Map<String, Object?>> decrypt(Row row) async {
    final box = SecretBox(
      row['ciphertext'] as Uint8List,
      nonce: row['nonce'] as Uint8List,
      mac: Mac(row['tag'] as Uint8List),
    );
    final plain = await aes.decrypt(
      box,
      secretKey: key,
      aad: aad(row['kind'] as String, row['opaque_key'] as String),
    );
    return Map<String, Object?>.from(jsonDecode(utf8.decode(plain)) as Map);
  }

  try {
    final db = sqlite3.open(path);
    database = db;
    db.execute('PRAGMA journal_mode=WAL');
    db.execute('PRAGMA temp_store=MEMORY');
    db.execute('PRAGMA synchronous=FULL');
    db.execute(
      'CREATE TABLE IF NOT EXISTS records(opaque_key TEXT PRIMARY KEY,kind TEXT NOT NULL,nonce BLOB NOT NULL,ciphertext BLOB NOT NULL,tag BLOB NOT NULL)',
    );
    startup.send(commands.sendPort);
    await for (final raw in commands) {
      final request = raw as List<Object?>;
      final id = request[0]! as int;
      final operation = request[1]! as String;
      try {
        if (request[3] != generation) throw StateError('stale');
        Object? result;
        if (const [
          'put_upload',
          'upload',
          'upload_file',
          'finish_upload',
        ].contains(operation)) {
          final recordKey = await opaque('upload', 'current');
          final rows = db.select('SELECT * FROM records WHERE opaque_key=?', [
            recordKey,
          ]);
          final previous = rows.isEmpty ? null : await decrypt(rows.single);
          if (operation == 'upload')
            result = previous;
          else {
            final input = Map<String, Object?>.from(request[2]! as Map);
            final payload = operation == 'upload_file'
                ? Map<String, Object?>.from(input['original']! as Map)
                : input;
            if (previous != null &&
                jsonEncode(canonical(previous)) !=
                    jsonEncode(canonical(payload)))
              throw StateError('conflict');
            if (operation != 'put_upload' && previous == null)
              throw StateError('missing');
            if (operation == 'upload_file' &&
                previous!['file_id'] != null &&
                previous['file_id'] != input['file_id'])
              throw StateError('conflict');
            final next = operation == 'upload_file'
                ? {...payload, 'file_id': input['file_id']}
                : payload;
            List<Object?>? write;
            if (operation != 'finish_upload' &&
                (previous == null || operation == 'upload_file')) {
              final box = await aes.encrypt(
                utf8.encode(jsonEncode(next)),
                secretKey: key,
                nonce: aes.newNonce(),
                aad: aad('upload', recordKey),
              );
              write = [
                recordKey,
                'upload',
                Uint8List.fromList(box.nonce),
                Uint8List.fromList(box.cipherText),
                Uint8List.fromList(box.mac.bytes),
              ];
            }
            db.execute('BEGIN IMMEDIATE');
            try {
              if (db
                  .select(
                    "SELECT 1 FROM records WHERE kind IN ('pending_send','action','device_revocation') LIMIT 1",
                  )
                  .isNotEmpty)
                throw StateError('conflict');
              if (operation == 'finish_upload')
                db.execute('DELETE FROM records WHERE opaque_key=?', [
                  recordKey,
                ]);
              else if (write != null)
                db.execute(
                  'INSERT OR REPLACE INTO records VALUES(?,?,?,?,?)',
                  write,
                );
              db.execute('COMMIT');
            } catch (_) {
              db.execute('ROLLBACK');
              rethrow;
            }
          }
        } else if (operation == 'put_device_revocation' ||
            operation == 'device_revocation' ||
            operation == 'finish_device_revocation') {
          final recordKey = await opaque('device_revocation', 'current');
          final rows = db.select('SELECT * FROM records WHERE opaque_key=?', [
            recordKey,
          ]);
          final previous = rows.isEmpty ? null : await decrypt(rows.single);
          if (operation == 'device_revocation')
            result = previous;
          else {
            final payload = Map<String, Object?>.from(request[2]! as Map);
            if (previous != null &&
                jsonEncode(canonical(previous)) !=
                    jsonEncode(canonical(payload)))
              throw StateError('conflict');
            if (operation == 'finish_device_revocation') {
              if (previous == null) throw StateError('conflict');
              db.execute('DELETE FROM records WHERE opaque_key=?', [recordKey]);
            } else if (previous == null) {
              final box = await aes.encrypt(
                utf8.encode(jsonEncode(payload)),
                secretKey: key,
                nonce: aes.newNonce(),
                aad: aad('device_revocation', recordKey),
              );
              db.execute('INSERT INTO records VALUES(?,?,?,?,?)', [
                recordKey,
                'device_revocation',
                Uint8List.fromList(box.nonce),
                Uint8List.fromList(box.cipherText),
                Uint8List.fromList(box.mac.bytes),
              ]);
            }
          }
        } else if (operation == 'put_action' ||
            operation == 'action' ||
            operation == 'finish_action') {
          final recordKey = await opaque('action', 'current');
          final rows = db.select('SELECT * FROM records WHERE opaque_key=?', [
            recordKey,
          ]);
          final previous = rows.isEmpty ? null : await decrypt(rows.single);
          if (operation == 'action')
            result = previous;
          else {
            final payload = Map<String, Object?>.from(request[2]! as Map);
            if (previous != null &&
                jsonEncode(canonical(previous)) !=
                    jsonEncode(canonical(payload)))
              throw StateError('conflict');
            if (operation == 'finish_action') {
              if (previous == null) throw StateError('conflict');
              db.execute('DELETE FROM records WHERE opaque_key=?', [recordKey]);
            } else if (previous == null) {
              final box = await aes.encrypt(
                utf8.encode(jsonEncode(payload)),
                secretKey: key,
                nonce: aes.newNonce(),
                aad: aad('action', recordKey),
              );
              db.execute('BEGIN IMMEDIATE');
              try {
                if (db
                    .select(
                      "SELECT 1 FROM records WHERE kind IN ('pending_send','upload') LIMIT 1",
                    )
                    .isNotEmpty)
                  throw StateError('conflict');
                db.execute('INSERT INTO records VALUES(?,?,?,?,?)', [
                  recordKey,
                  'action',
                  Uint8List.fromList(box.nonce),
                  Uint8List.fromList(box.cipherText),
                  Uint8List.fromList(box.mac.bytes),
                ]);
                db.execute('COMMIT');
              } catch (_) {
                db.execute('ROLLBACK');
                rethrow;
              }
            }
          }
        } else if (operation == 'put_registration' ||
            operation == 'registration' ||
            operation == 'finish_registration') {
          final recordKey = await opaque('registration', 'current');
          final rows = db.select('SELECT * FROM records WHERE opaque_key=?', [
            recordKey,
          ]);
          final previous = rows.isEmpty ? null : await decrypt(rows.single);
          if (operation == 'registration') {
            result = previous;
          } else if (operation == 'finish_registration') {
            if (previous != null && previous['registration_id'] != request[2])
              throw StateError('conflict');
            db.execute('DELETE FROM records WHERE opaque_key=?', [recordKey]);
          } else {
            final payload = Map<String, Object?>.from(request[2]! as Map);
            if (previous != null) {
              if (jsonEncode(canonical(previous)) !=
                  jsonEncode(canonical(payload)))
                throw StateError('conflict');
            } else {
              final box = await aes.encrypt(
                utf8.encode(jsonEncode(payload)),
                secretKey: key,
                nonce: aes.newNonce(),
                aad: aad('registration', recordKey),
              );
              db.execute('INSERT INTO records VALUES(?,?,?,?,?)', [
                recordKey,
                'registration',
                Uint8List.fromList(box.nonce),
                Uint8List.fromList(box.cipherText),
                Uint8List.fromList(box.mac.bytes),
              ]);
            }
          }
        } else if (operation == 'put_pending') {
          final payload = Map<String, Object?>.from(request[2]! as Map);
          final logical = payload['client_msg_id']! as String;
          final recordKey = await opaque('pending_send', logical);
          final current = db.select(
            'SELECT * FROM records WHERE opaque_key=?',
            [recordKey],
          );
          if (current.isNotEmpty) {
            if (jsonEncode(canonical(await decrypt(current.single))) !=
                jsonEncode(canonical(payload)))
              throw StateError('conflict');
          } else {
            final box = await aes.encrypt(
              utf8.encode(jsonEncode(payload)),
              secretKey: key,
              nonce: aes.newNonce(),
              aad: aad('pending_send', recordKey),
            );
            db.execute('BEGIN IMMEDIATE');
            try {
              if (db
                  .select("SELECT 1 FROM records WHERE kind='action' LIMIT 1")
                  .isNotEmpty)
                throw StateError('conflict');
              final uploads = db.select(
                "SELECT * FROM records WHERE kind='upload'",
              );
              if (uploads.isNotEmpty) {
                final upload = await decrypt(uploads.single);
                if (payload['file_id'] == null ||
                    payload['file_id'] != upload['file_id'] ||
                    payload['upload_request_id'] !=
                        upload['upload_request_id'] ||
                    payload['conversation_id'] != upload['conversation_id'] ||
                    payload['acting_membership_id'] !=
                        upload['acting_membership_id'])
                  throw StateError('conflict');
              } else if (payload['file_id'] != null)
                throw StateError('conflict');
              db.execute('INSERT INTO records VALUES(?,?,?,?,?)', [
                recordKey,
                'pending_send',
                Uint8List.fromList(box.nonce),
                Uint8List.fromList(box.cipherText),
                Uint8List.fromList(box.mac.bytes),
              ]);
              db.execute('COMMIT');
            } catch (_) {
              db.execute('ROLLBACK');
              rethrow;
            }
          }
        } else if (operation == 'acknowledge_send' ||
            operation == 'abandon_send') {
          final payload = Map<String, Object?>.from(request[2]! as Map);
          final pending = operation == 'abandon_send'
              ? payload
              : Map<String, Object?>.from(payload['pending']! as Map);
          final logical = pending['client_msg_id']! as String;
          final recordKey = await opaque('pending_send', logical);
          final rows = db.select('SELECT * FROM records WHERE opaque_key=?', [
            recordKey,
          ]);
          if (rows.isEmpty ||
              jsonEncode(canonical(await decrypt(rows.single))) !=
                  jsonEncode(canonical(pending)))
            throw StateError('conflict');
          List<Object?>? write;
          if (operation == 'acknowledge_send') {
            final value = Map<String, Object?>.from(
              payload['confirmation']! as Map,
            );
            final confirmationKey = await opaque('send_confirmation', logical);
            final box = await aes.encrypt(
              utf8.encode(jsonEncode(value)),
              secretKey: key,
              nonce: aes.newNonce(),
              aad: aad('send_confirmation', confirmationKey),
            );
            write = [
              confirmationKey,
              'send_confirmation',
              Uint8List.fromList(box.nonce),
              Uint8List.fromList(box.cipherText),
              Uint8List.fromList(box.mac.bytes),
            ];
          }
          db.execute('BEGIN IMMEDIATE');
          try {
            if (write != null)
              db.execute('INSERT INTO records VALUES(?,?,?,?,?)', write);
            if (pending['file_id'] != null) {
              final uploads = db.select(
                "SELECT * FROM records WHERE kind='upload'",
              );
              if (uploads.length != 1) throw StateError('missing');
              final upload = await decrypt(uploads.single);
              if (upload['file_id'] != pending['file_id'] ||
                  upload['upload_request_id'] != pending['upload_request_id'])
                throw StateError('conflict');
              db.execute('DELETE FROM records WHERE opaque_key=?', [
                uploads.single['opaque_key'],
              ]);
            }
            db.execute('DELETE FROM records WHERE opaque_key=?', [recordKey]);
            db.execute('COMMIT');
          } catch (_) {
            db.execute('ROLLBACK');
            rethrow;
          }
        } else if (operation == 'send_confirmation') {
          final recordKey = await opaque(
            'send_confirmation',
            request[2]! as String,
          );
          final rows = db.select('SELECT * FROM records WHERE opaque_key=?', [
            recordKey,
          ]);
          result = rows.isEmpty ? null : await decrypt(rows.single);
        } else if (operation == 'pending') {
          final values = <Map<String, Object?>>[];
          for (final row in db.select(
            "SELECT * FROM records WHERE kind='pending_send' ORDER BY opaque_key",
          )) {
            values.add(await decrypt(row));
          }
          result = values;
        } else if (operation == 'commit_page') {
          final payload = Map<String, Object?>.from(request[2]! as Map);
          final conversation = payload['conversation_id']! as String;
          final cursorKey = await opaque('cursor', conversation);
          final currentRows = db.select(
            'SELECT * FROM records WHERE opaque_key=?',
            [cursorKey],
          );
          final after = Seq.fromWire(payload['after_seq']);
          final next = Seq.fromWire(payload['next_after_seq']);
          var retainedNext = next;
          if (currentRows.isEmpty) {
            if (after.value != BigInt.zero) throw StateError('gap');
          } else {
            final current = await decrypt(currentRows.single);
            final old = Seq.fromWire(current['next_after_seq']);
            if (current['acting_membership_id'] !=
                    payload['acting_membership_id'] ||
                Seq.fromWire(
                      payload['receipt_revision'],
                    ).compareTo(Seq.fromWire(current['receipt_revision'])) <
                    0 ||
                after.compareTo(old) > 0 ||
                (after != old && next.compareTo(old) > 0))
              throw StateError('gap or scope');
            if (old.compareTo(next) > 0) retainedNext = old;
          }
          final writes = <List<Object?>>[];
          final deletes = <String>[];
          Future<void> prepare(
            String kind,
            String logical,
            Map<String, Object?> value,
          ) async {
            final recordKey = await opaque(kind, logical);
            final box = await aes.encrypt(
              utf8.encode(jsonEncode(value)),
              secretKey: key,
              nonce: aes.newNonce(),
              aad: aad(kind, recordKey),
            );
            writes.add([
              recordKey,
              kind,
              Uint8List.fromList(box.nonce),
              Uint8List.fromList(box.cipherText),
              Uint8List.fromList(box.mac.bytes),
            ]);
          }

          for (final rawRow in payload['messages']! as List) {
            final row = Map<String, Object?>.from(rawRow as Map);
            final logical = '$conversation/${row['seq']}';
            if (row['value'] == null) {
              deletes.add(await opaque('message', logical));
            } else {
              await prepare(
                'message',
                logical,
                Map<String, Object?>.from(row['value']! as Map),
              );
            }
          }
          await prepare('cursor', conversation, {
            'conversation_id': conversation,
            'acting_membership_id': payload['acting_membership_id'],
            'generation': generation,
            'next_after_seq': retainedNext.toString(),
            'receipt_revision': payload['receipt_revision'],
          });
          await prepare(
            'receipt',
            '$conversation/$next',
            Map<String, Object?>.from(payload['receipt']! as Map),
          );
          db.execute('BEGIN IMMEDIATE');
          try {
            for (final recordKey in deletes) {
              db.execute('DELETE FROM records WHERE opaque_key=?', [recordKey]);
            }
            for (final row in writes) {
              db.execute(
                'INSERT OR REPLACE INTO records VALUES(?,?,?,?,?)',
                row,
              );
            }
            db.execute('COMMIT');
          } catch (_) {
            db.execute('ROLLBACK');
            rethrow;
          }
        } else if (operation == 'clear_received') {
          db.execute(
            "DELETE FROM records WHERE kind IN ('message','cursor','receipt')",
          );
        } else if (operation == 'message_candidates') {
          final rows = <Map<String, Object?>>[];
          for (final row in db.select(
            "SELECT * FROM records WHERE kind='message'",
          )) {
            rows.add({
              'value': await decrypt(row),
              'opaque_key': row['opaque_key'],
              'nonce': row['nonce'],
            });
          }
          result = rows;
        } else if (operation == 'purge_messages') {
          db.execute('BEGIN IMMEDIATE');
          try {
            for (final raw in request[2]! as List) {
              final row = Map<String, Object?>.from(raw as Map);
              db.execute(
                "DELETE FROM records WHERE kind='message' AND opaque_key=? AND nonce=?",
                [row['opaque_key'], row['nonce']],
              );
            }
            db.execute('COMMIT');
          } catch (_) {
            db.execute('ROLLBACK');
            rethrow;
          }
        } else if (operation == 'acknowledge_page') {
          final payload = Map<String, Object?>.from(request[2]! as Map);
          final conversation = payload['conversation_id']! as String;
          final next = Seq.fromWire(payload['next_after_seq']);
          final deletes = <String>[];
          for (final row in db.select(
            "SELECT * FROM records WHERE kind='receipt'",
          )) {
            final receipt = await decrypt(row);
            if (receipt['conversation_id'] == conversation &&
                Seq.fromWire(receipt['next_after_seq']).compareTo(next) <= 0) {
              deletes.add(row['opaque_key'] as String);
            }
          }
          db.execute('BEGIN IMMEDIATE');
          try {
            for (final recordKey in deletes) {
              db.execute('DELETE FROM records WHERE opaque_key=?', [recordKey]);
            }
            db.execute('COMMIT');
          } catch (_) {
            db.execute('ROLLBACK');
            rethrow;
          }
        } else if (operation == 'cursor') {
          final recordKey = await opaque('cursor', request[2]! as String);
          final rows = db.select('SELECT * FROM records WHERE opaque_key=?', [
            recordKey,
          ]);
          result = rows.isEmpty
              ? null
              : (await decrypt(rows.single))['next_after_seq'];
        } else if (operation == 'receipts' || operation == 'messages') {
          final kind = operation == 'receipts' ? 'receipt' : 'message';
          final values = <Map<String, Object?>>[];
          for (final row in db.select(
            'SELECT * FROM records WHERE kind=? ORDER BY opaque_key',
            [kind],
          )) {
            values.add(await decrypt(row));
          }
          result = values;
        } else if (operation == 'close' || operation == 'erase') {
          db.execute('PRAGMA wal_checkpoint(TRUNCATE)');
          db.close();
          database = null;
          if (operation == 'erase') {
            for (final suffix in ['', '-wal', '-shm']) {
              final file = File('$path$suffix');
              if (await file.exists()) await file.delete();
            }
          }
          eraseKey();
          replies.send([id, true, null]);
          break;
        } else {
          throw StateError('invalid');
        }
        replies.send([id, true, result]);
      } catch (_) {
        replies.send([id, false, null]);
      }
    }
  } catch (_) {
    startup.send(null);
  } finally {
    database?.close();
    commands.close();
    eraseKey();
  }
}
