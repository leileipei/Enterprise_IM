import Foundation
import Darwin

// A descriptor pins the selected folder. Only a newly created, private staging
// inode is writable; the final rename refuses an existing destination atomically.
final class NativeExportSession {
  enum Failure: Error { case unavailable, invalidName, exists, invalidState, tooLarge }
  private var directoryFD: Int32 = -1
  private var fileFD: Int32 = -1
  private var identity = stat()
  private let name: String
  private let staging = ".enterprise-im-export-" + UUID().uuidString
  private var count = 0

  static func validName(_ name: String) -> Bool {
    return !name.isEmpty && name != "." && name != ".." && name.utf8.count <= 255
      && name == name.trimmingCharacters(in: .whitespacesAndNewlines)
      && !name.contains(where: { "/\\:".contains($0) })
      && name.unicodeScalars.allSatisfy { !CharacterSet.controlCharacters.contains($0) }
  }

  init(directory: URL, name: String) throws {
    self.name = name
    guard Self.validName(name), directory.isFileURL else { throw Failure.invalidName }
    directoryFD = open(directory.path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW | O_CLOEXEC)
    guard directoryFD >= 0 else { throw Failure.unavailable }
    var existing = stat()
    guard fstatat(directoryFD, name, &existing, AT_SYMLINK_NOFOLLOW) != 0, errno == ENOENT else {
      throw Failure.exists
    }
    fileFD = openat(directoryFD, staging, O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC, 0o600)
    guard fileFD >= 0, fstat(fileFD, &identity) == 0 else { throw Failure.unavailable }
  }

  func write(_ data: Data) throws {
    guard fileFD >= 0 else { throw Failure.invalidState }
    guard data.count <= 32768, count + data.count <= 25 * 1024 * 1024 else { throw Failure.tooLarge }
    try data.withUnsafeBytes { buffer in
      var written = 0
      while written < buffer.count {
        let size = Darwin.write(fileFD, buffer.baseAddress!.advanced(by: written), buffer.count - written)
        if size < 0 && errno == EINTR { continue }
        guard size > 0 else { throw Failure.unavailable }
        written += size
      }
    }
    count += data.count
  }

  private func ownsStaging() -> Bool {
    var current = stat()
    return directoryFD >= 0 && identity.st_ino != 0
      && fstatat(directoryFD, staging, &current, AT_SYMLINK_NOFOLLOW) == 0
      && (current.st_mode & S_IFMT) == S_IFREG
      && current.st_dev == identity.st_dev && current.st_ino == identity.st_ino
  }

  func finish() throws {
    guard fileFD >= 0, count > 0, ownsStaging() else { throw Failure.invalidState }
    guard fsync(fileFD) == 0 else { throw Failure.unavailable }
    // RENAME_EXCL guarantees that a file appearing after selection is preserved.
    guard renameatx_np(directoryFD, staging, directoryFD, name, UInt32(RENAME_EXCL)) == 0 else {
      throw errno == EEXIST ? Failure.exists : Failure.unavailable
    }
    close(fileFD); fileFD = -1
    close(directoryFD); directoryFD = -1
  }

  func abort() throws {
    if fileFD >= 0 { close(fileFD); fileFD = -1 }
    if ownsStaging(), unlinkat(directoryFD, staging, 0) != 0 { throw Failure.unavailable }
    if directoryFD >= 0 { close(directoryFD); directoryFD = -1 }
  }

  deinit {
    try? abort()
    if directoryFD >= 0 { close(directoryFD) }
  }
}
