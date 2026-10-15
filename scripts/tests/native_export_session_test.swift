import Foundation
import Darwin

@main struct NativeExportSessionTests {
  static func main() {
    do { try run() } catch {
      fputs("FAIL native export: \(error)\n", stderr)
      exit(1)
    }
  }
  enum ProbeFailure: Error { case expectedRejection }
  static func rejects(_ action: () throws -> Void) throws {
    var rejected = false
    do { try action() } catch { rejected = true }
    if !rejected { throw ProbeFailure.expectedRejection }
  }
  static func run() throws {
    let root = FileManager.default.temporaryDirectory.appendingPathComponent("im-export-test-" + UUID().uuidString)
    let fm = FileManager.default
    try fm.createDirectory(at: root, withIntermediateDirectories: false)
    defer { chmod(root.path, 0o700); try? fm.removeItem(at: root) }
    print("OWNED_EXPORT_FIXTURE " + root.path)
    let target = root.appendingPathComponent("说明.txt")
    let session = try NativeExportSession(directory: root, name: "说明.txt")
    try session.write(Data([97, 98, 99]))
    try session.finish()
    let actual = try Data(contentsOf: target)
    precondition(actual == Data([97, 98, 99]))
    let files = try fm.contentsOfDirectory(atPath: root.path)
    precondition(files == ["说明.txt"])
    print("PASS exact bytes and no staging remainder")
    try rejects { _ = try NativeExportSession(directory: root, name: "说明.txt") }
    precondition(tryRead(target) == Data([97, 98, 99]))
    print("PASS existing target is preserved")
    let cancelled = try NativeExportSession(directory: root, name: "cancelled.txt")
    try cancelled.write(Data([1, 2]))
    try cancelled.abort(); try cancelled.abort()
    try rejects { try cancelled.write(Data([3])) }
    let afterCancel = try fm.contentsOfDirectory(atPath: root.path)
    precondition(afterCancel == ["说明.txt"])
    print("PASS abort is idempotent and removes only own staging")
    let collision = try NativeExportSession(directory: root, name: "collision.txt")
    try collision.write(Data([9]))
    let collided = root.appendingPathComponent("collision.txt")
    try Data([4, 5]).write(to: collided)
    try rejects { try collision.finish() }
    try collision.abort()
    precondition(tryRead(collided) == Data([4, 5]))
    print("PASS commit-time collision never replaces existing content")
    let symlink = root.appendingPathComponent("link.txt")
    try fm.createSymbolicLink(at: symlink, withDestinationURL: target)
    try rejects { _ = try NativeExportSession(directory: root, name: "link.txt") }
    precondition(tryRead(target) == Data([97, 98, 99]))
    print("PASS symlink target is not followed")
    for name in ["", ".", "..", "../escape", "a/b", "a\\b", "a:b", "a\n", " a", String(repeating: "字", count: 86)] {
      try rejects { _ = try NativeExportSession(directory: root, name: name) }
    }
    print("PASS invalid leaf names are rejected")
    let empty = try NativeExportSession(directory: root, name: "empty.txt")
    try rejects { try empty.finish() }; try empty.abort()
    let tooLarge = try NativeExportSession(directory: root, name: "large.txt")
    try rejects { try tooLarge.write(Data(repeating: 0, count: 32769)) }; try tooLarge.abort()
    let bounded = try NativeExportSession(directory: root, name: "bounded.txt")
    for _ in 0..<800 { try bounded.write(Data(repeating: 7, count: 32768)) }
    try rejects { try bounded.write(Data([1])) }; try bounded.abort()
    print("PASS empty, oversized chunk and total size limits")
    let denied = try NativeExportSession(directory: root, name: "denied.txt")
    try denied.write(Data([6]))
    precondition(chmod(root.path, 0o500) == 0)
    try rejects { try denied.abort() }
    precondition(chmod(root.path, 0o700) == 0)
    try denied.abort()
    let remnants = try fm.contentsOfDirectory(atPath: root.path)
    precondition(!remnants.contains(where: { $0.hasPrefix(".enterprise-im-export-") }))
    print("PASS cleanup failure is reported and retry removes own staging")
    let tampered = try NativeExportSession(directory: root, name: "tampered.txt")
    try tampered.write(Data([8]))
    let staging = try fm.contentsOfDirectory(at: root, includingPropertiesForKeys: nil).filter { $0.lastPathComponent.hasPrefix(".enterprise-im-export-") }
    precondition(staging.count == 1)
    try fm.removeItem(at: staging[0])
    try fm.createSymbolicLink(at: staging[0], withDestinationURL: target)
    try rejects { try tampered.finish() }; try tampered.abort()
    let type = try fm.attributesOfItem(atPath: staging[0].path)[.type] as? FileAttributeType
    precondition(type == .typeSymbolicLink)
    precondition(tryRead(target) == Data([97, 98, 99]))
    print("PASS substituted staging is neither committed nor deleted")
  }
  static func tryRead(_ url: URL) -> Data { try! Data(contentsOf: url) }
}
